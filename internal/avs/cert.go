package avs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	caLife = 20 * 365 * 24 * time.Hour

	caCommonName = "overdub AVS relay CA"
	leafLife     = 20 * 365 * 24 * time.Hour

	TrustDir     = "/system/etc/security/cacerts"
	TrustDirSHA1 = "/system/etc/security/cacerts-sha1"
)

func Identity(bundlePath string) (tls.Certificate, []byte, error) {
	if raw, err := os.ReadFile(bundlePath); err == nil {
		cert, ca, err := parseBundle(raw)
		if err == nil {
			return cert, ca, nil
		}
	}
	cert, ca, bundle, err := mint()
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(bundlePath), 0o700); err != nil {
		return tls.Certificate{}, nil, err
	}
	if err := os.WriteFile(bundlePath, bundle, 0o600); err != nil {
		return tls.Certificate{}, nil, err
	}
	return cert, ca, nil
}

func parseBundle(raw []byte) (tls.Certificate, []byte, error) {
	var certs [][]byte
	var keyDER []byte
	var caPEM []byte
	rest := raw
	for {
		block, more := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = more
		switch block.Type {
		case "CERTIFICATE":
			certs = append(certs, block.Bytes)
			if len(certs) == 2 {
				caPEM = pem.EncodeToMemory(block)
			}
		case "EC PRIVATE KEY":
			keyDER = block.Bytes
		}
	}
	if len(certs) != 2 || keyDER == nil {
		return tls.Certificate{}, nil, fmt.Errorf("avs: the identity bundle is not a leaf, a CA and a key")
	}
	leaf, err := x509.ParseCertificate(certs[0])
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	if time.Now().After(leaf.NotAfter) {
		return tls.Certificate{}, nil, fmt.Errorf("avs: the identity expired on %s", leaf.NotAfter)
	}
	key, err := x509.ParseECPrivateKey(keyDER)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	return tls.Certificate{Certificate: certs, PrivateKey: key, Leaf: leaf}, caPEM, nil
}

func mint() (tls.Certificate, []byte, []byte, error) {
	fail := func(err error) (tls.Certificate, []byte, []byte, error) {
		return tls.Certificate{}, nil, nil, err
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fail(err)
	}
	now := time.Now().Add(-time.Hour)
	caTemplate := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: caCommonName},
		NotBefore:             now,
		NotAfter:              now.Add(caLife),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return fail(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return fail(err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fail(err)
	}
	host, _, err := net.SplitHostPort(Endpoint)
	if err != nil {
		return fail(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now,
		NotAfter:     now.Add(leafLife),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host, "localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		return fail(err)
	}
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return fail(err)
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	bundle := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), caPEM...)
	bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER})...)

	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return fail(err)
	}
	cert := tls.Certificate{
		Certificate: [][]byte{leafDER, caDER},
		PrivateKey:  leafKey,
		Leaf:        leaf,
	}
	return cert, caPEM, bundle, nil
}

func serial() *big.Int {
	b := make([]byte, 16)
	rand.Read(b)
	return new(big.Int).SetBytes(b)
}

func TrustName(caPEM []byte) (string, error) {
	block, _ := pem.Decode(caPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("avs: that is not a PEM certificate")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	sum := md5.Sum(ca.RawSubject)
	return fmt.Sprintf("%08x.0", binary.LittleEndian.Uint32(sum[:4])), nil
}

func TrustNameSHA1(caPEM []byte) (string, error) {
	block, _ := pem.Decode(caPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("avs: that is not a PEM certificate")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	var rdns pkix.RDNSequence
	if rest, err := asn1.Unmarshal(ca.RawSubject, &rdns); err != nil || len(rest) > 0 {
		return "", fmt.Errorf("avs: the subject does not parse: %v", err)
	}
	var canon []byte
	for _, rdn := range rdns {
		var set []byte
		for _, atv := range rdn {
			value, ok := atv.Value.(string)
			if !ok {
				return "", fmt.Errorf("avs: %v in the subject is not a string", atv.Type)
			}
			value = strings.ToLower(strings.Join(strings.Fields(value), " "))
			seq, err := asn1.Marshal(struct {
				Type  asn1.ObjectIdentifier
				Value asn1.RawValue
			}{atv.Type, asn1.RawValue{Tag: asn1.TagUTF8String, Bytes: []byte(value)}})
			if err != nil {
				return "", err
			}
			set = append(set, seq...)
		}
		wrapped, err := asn1.Marshal(asn1.RawValue{Tag: asn1.TagSet, IsCompound: true, Bytes: set})
		if err != nil {
			return "", err
		}
		canon = append(canon, wrapped...)
	}
	sum := sha1.Sum(canon)
	return fmt.Sprintf("%08x.0", binary.LittleEndian.Uint32(sum[:4])), nil
}

func TrustPaths(caPEM []byte) ([]string, error) {
	old, err := TrustName(caPEM)
	if err != nil {
		return nil, err
	}
	current, err := TrustNameSHA1(caPEM)
	if err != nil {
		return nil, err
	}
	return []string{filepath.Join(TrustDir, old), filepath.Join(TrustDirSHA1, current)}, nil
}

func TrustFile(caPEM []byte) ([]byte, error) {
	block, _ := pem.Decode(caPEM)
	if block == nil {
		return nil, fmt.Errorf("avs: that is not a PEM certificate")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	text := strings.Builder{}
	text.Write(caPEM)
	fmt.Fprintf(&text, "Subject: %s\n", ca.Subject)
	fmt.Fprintf(&text, "Serial: %x\n", ca.SerialNumber)
	return []byte(text.String()), nil
}
