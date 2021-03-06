// Copyright 2019 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not
// use this file except in compliance with the License. You may obtain a copy of
// the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations under
// the License.

package attest

import (
	"bytes"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"github.com/google/certificate-transparency-go/asn1"
	"github.com/google/certificate-transparency-go/x509"

	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpmutil"
)

const (
	tpmPtManufacturer = 0x00000100 + 5  // PT_FIXED + offset of 5
	tpmPtVendorString = 0x00000100 + 6  // PT_FIXED + offset of 6
	tpmPtFwVersion1   = 0x00000100 + 11 // PT_FIXED + offset of 11

	// Defined in "Registry of reserved TPM 2.0 handles and localities".
	nvramCertIndex = 0x1c00002

	// Defined in "Registry of reserved TPM 2.0 handles and localities", and checked on a glinux machine.
	commonSrkEquivalentHandle = 0x81000001
	commonEkEquivalentHandle  = 0x81010001
)

var (
	aikTemplate = tpm2.Public{
		Type:       tpm2.AlgRSA,
		NameAlg:    tpm2.AlgSHA256,
		Attributes: tpm2.FlagSignerDefault,
		RSAParameters: &tpm2.RSAParams{
			Sign: &tpm2.SigScheme{
				Alg:  tpm2.AlgRSASSA,
				Hash: tpm2.AlgSHA256,
			},
			KeyBits: 2048,
		},
	}
	defaultSRKTemplate = tpm2.Public{
		Type:       tpm2.AlgRSA,
		NameAlg:    tpm2.AlgSHA256,
		Attributes: tpm2.FlagStorageDefault | tpm2.FlagNoDA,
		RSAParameters: &tpm2.RSAParams{
			Symmetric: &tpm2.SymScheme{
				Alg:     tpm2.AlgAES,
				KeyBits: 128,
				Mode:    tpm2.AlgCFB,
			},
			ModulusRaw: make([]byte, 256),
			KeyBits:    2048,
		},
	}
	// Default EK template defined in:
	// https://trustedcomputinggroup.org/wp-content/uploads/Credential_Profile_EK_V2.0_R14_published.pdf
	defaultEKTemplate = tpm2.Public{
		Type:    tpm2.AlgRSA,
		NameAlg: tpm2.AlgSHA256,
		Attributes: tpm2.FlagFixedTPM | tpm2.FlagFixedParent | tpm2.FlagSensitiveDataOrigin |
			tpm2.FlagAdminWithPolicy | tpm2.FlagRestricted | tpm2.FlagDecrypt,
		AuthPolicy: []byte{
			0x83, 0x71, 0x97, 0x67, 0x44, 0x84,
			0xB3, 0xF8, 0x1A, 0x90, 0xCC, 0x8D,
			0x46, 0xA5, 0xD7, 0x24, 0xFD, 0x52,
			0xD7, 0x6E, 0x06, 0x52, 0x0B, 0x64,
			0xF2, 0xA1, 0xDA, 0x1B, 0x33, 0x14,
			0x69, 0xAA,
		},
		RSAParameters: &tpm2.RSAParams{
			Symmetric: &tpm2.SymScheme{
				Alg:     tpm2.AlgAES,
				KeyBits: 128,
				Mode:    tpm2.AlgCFB,
			},
			KeyBits:    2048,
			ModulusRaw: make([]byte, 256),
		},
	}
)

type tpm20Info struct {
	vendor       string
	manufacturer TCGVendorID
	fwMajor      int
	fwMinor      int
}

func readTPM2VendorAttributes(tpm io.ReadWriter) (tpm20Info, error) {
	var vendorInfo string
	// The Vendor String is split up into 4 sections of 4 bytes,
	// for a maximum length of 16 octets of ASCII text. We iterate
	// through the 4 indexes to get all 16 bytes & construct vendorInfo.
	// See: TPM_PT_VENDOR_STRING_1 in TPM 2.0 Structures reference.
	for i := 0; i < 4; i++ {
		caps, _, err := tpm2.GetCapability(tpm, tpm2.CapabilityTPMProperties, 1, tpmPtVendorString+uint32(i))
		if err != nil {
			return tpm20Info{}, fmt.Errorf("tpm2.GetCapability(PT_VENDOR_STRING_%d) failed: %v", i+1, err)
		}
		subset, ok := caps[0].(tpm2.TaggedProperty)
		if !ok {
			return tpm20Info{}, fmt.Errorf("got capability of type %T, want tpm2.TaggedProperty", caps[0])
		}
		// Reconstruct the 4 ASCII octets from the uint32 value.
		vendorInfo += string(subset.Value&0xFF000000) + string(subset.Value&0xFF0000) + string(subset.Value&0xFF00) + string(subset.Value&0xFF)
	}

	caps, _, err := tpm2.GetCapability(tpm, tpm2.CapabilityTPMProperties, 1, tpmPtManufacturer)
	if err != nil {
		return tpm20Info{}, fmt.Errorf("tpm2.GetCapability(PT_MANUFACTURER) failed: %v", err)
	}
	manu, ok := caps[0].(tpm2.TaggedProperty)
	if !ok {
		return tpm20Info{}, fmt.Errorf("got capability of type %T, want tpm2.TaggedProperty", caps[0])
	}

	caps, _, err = tpm2.GetCapability(tpm, tpm2.CapabilityTPMProperties, 1, tpmPtFwVersion1)
	if err != nil {
		return tpm20Info{}, fmt.Errorf("tpm2.GetCapability(PT_FIRMWARE_VERSION_1) failed: %v", err)
	}
	fw, ok := caps[0].(tpm2.TaggedProperty)
	if !ok {
		return tpm20Info{}, fmt.Errorf("got capability of type %T, want tpm2.TaggedProperty", caps[0])
	}

	return tpm20Info{
		vendor:       strings.Trim(vendorInfo, "\x00"),
		manufacturer: TCGVendorID(manu.Value),
		fwMajor:      int((fw.Value & 0xffff0000) >> 16),
		fwMinor:      int(fw.Value & 0x0000ffff),
	}, nil
}

// ParseEKCertificate parses a raw DER encoded EK certificate blob.
func ParseEKCertificate(ekCert []byte) (*x509.Certificate, error) {
	var wasWrapped bool

	// TCG PC Specific Implementation section 7.3.2 specifies
	// a prefix when storing a certificate in NVRAM. We look
	// for and unwrap the certificate if its present.
	if len(ekCert) > 5 && bytes.Equal(ekCert[:3], []byte{0x10, 0x01, 0x00}) {
		certLen := binary.BigEndian.Uint16(ekCert[3:5])
		if len(ekCert) < int(certLen+5) {
			return nil, fmt.Errorf("parsing nvram header: ekCert size %d smaller than specified cert length %d", len(ekCert), certLen)
		}
		ekCert = ekCert[5 : 5+certLen]
		wasWrapped = true
	}

	// If the cert parses fine without any changes, we are G2G.
	if c, err := x509.ParseCertificate(ekCert); err == nil {
		return c, nil
	}
	// There might be trailing nonsense in the cert, which Go
	// does not parse correctly. As ASN1 data is TLV encoded, we should
	// be able to just get the certificate, and then send that to Go's
	// certificate parser.
	var cert struct {
		Raw asn1.RawContent
	}
	if _, err := asn1.UnmarshalWithParams(ekCert, &cert, "lax"); err != nil && x509.IsFatal(err) {
		return nil, fmt.Errorf("asn1.Unmarshal() failed: %v, wasWrapped=%v", err, wasWrapped)
	}

	c, err := x509.ParseCertificate(cert.Raw)
	if err != nil && x509.IsFatal(err) {
		return nil, fmt.Errorf("x509.ParseCertificate() failed: %v", err)
	}
	return c, nil
}

const (
	manufacturerIntel     = "Intel"
	intelEKCertServiceURL = "https://ekop.intel.com/ekcertservice/"
)

func intelEKURL(ekPub *rsa.PublicKey) string {
	pubHash := sha256.New()
	pubHash.Write(ekPub.N.Bytes())
	pubHash.Write([]byte{0x1, 0x00, 0x01})

	return intelEKCertServiceURL + base64.URLEncoding.EncodeToString(pubHash.Sum(nil))
}

func readEKCertFromNVRAM20(tpm io.ReadWriter) (*x509.Certificate, error) {
	ekCert, err := tpm2.NVReadEx(tpm, nvramCertIndex, tpm2.HandleOwner, "", 0)
	if err != nil {
		return nil, fmt.Errorf("reading EK cert: %v", err)
	}
	return ParseEKCertificate(ekCert)
}

func quote20(tpm io.ReadWriter, aikHandle tpmutil.Handle, hashAlg tpm2.Algorithm, nonce []byte) (*Quote, error) {
	sel := tpm2.PCRSelection{Hash: hashAlg}
	numPCRs := 24
	for pcr := 0; pcr < numPCRs; pcr++ {
		sel.PCRs = append(sel.PCRs, pcr)
	}

	quote, sig, err := tpm2.Quote(tpm, aikHandle, "", "", nonce, sel, tpm2.AlgNull)
	if err != nil {
		return nil, err
	}

	rawSig, err := tpmutil.Pack(sig.Alg, sig.RSA.HashAlg, sig.RSA.Signature)
	return &Quote{
		Version:   TPMVersion20,
		Quote:     quote,
		Signature: rawSig,
	}, err
}

func readAllPCRs20(tpm io.ReadWriter, alg tpm2.Algorithm) (map[uint32][]byte, error) {
	numPCRs := 24
	out := map[uint32][]byte{}

	// The TPM 2.0 spec says that the TPM can partially fulfill the
	// request. As such, we repeat the command up to 8 times to get all
	// 24 PCRs.
	for i := 0; i < numPCRs; i++ {
		// Build a selection structure, specifying all PCRs we do
		// not have the value for.
		sel := tpm2.PCRSelection{Hash: alg}
		for pcr := 0; pcr < numPCRs; pcr++ {
			if _, present := out[uint32(pcr)]; !present {
				sel.PCRs = append(sel.PCRs, pcr)
			}
		}

		// Ask the TPM for those PCR values.
		ret, err := tpm2.ReadPCRs(tpm, sel)
		if err != nil {
			return nil, fmt.Errorf("tpm2.ReadPCRs(%+v) failed with err: %v", sel, err)
		}
		// Keep track of the PCRs we were actually given.
		for pcr, digest := range ret {
			out[uint32(pcr)] = digest
		}
		if len(out) == numPCRs {
			break
		}
	}

	if len(out) != numPCRs {
		return nil, fmt.Errorf("failed to read all PCRs, only read %d", len(out))
	}

	return out, nil
}

// LoadAIK loads a previously-created aik into the TPM for use.
// A key loaded via this function needs to be closed with .Close().
// Only blobs generated by calling AIK.Serialize() are valid parameters
// to this function.
func (t *TPM) LoadAIK(opaqueBlob []byte) (*AIK, error) {
	return t.loadAIK(opaqueBlob)
}
