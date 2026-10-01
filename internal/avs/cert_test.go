package avs

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
)

func TestTrustNameMatchesOpenSSL(t *testing.T) {
	caPEM, err := os.ReadFile("testdata/known-ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	got, err := TrustName(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	const want = "db1b138a.0"
	if got != want {
		t.Errorf("TrustName = %s, want %s: the device reads a CA only under the name "+
			"openssl -subject_hash_old gives it, and a wrong name is a silently untrusted CA", got, want)
	}
}

func TestIdentityIsReusedNotReminted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity")
	first, firstCA, err := Identity(path)
	if err != nil {
		t.Fatal(err)
	}
	second, secondCA, err := Identity(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstCA) != string(secondCA) {
		t.Error("the second call minted a new CA: the one installed in the trust store " +
			"would no longer sign the certificate being served")
	}
	if first.Leaf.SerialNumber.Cmp(second.Leaf.SerialNumber) != 0 {
		t.Error("the second call minted a new leaf")
	}
}

func TestIdentityServesTheEndpointName(t *testing.T) {
	cert, caPEM, err := Identity(filepath.Join(t.TempDir(), "identity"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.Leaf.VerifyHostname("avs-alexa-4-na.amazon.com"); err != nil {
		t.Errorf("the leaf does not name the endpoint: %v", err)
	}
	if len(cert.Certificate) != 2 {
		t.Fatalf("served %d certificates, want the leaf and its CA: a device that is "+
			"given only the leaf cannot build a chain to the CA it trusts", len(cert.Certificate))
	}
	if _, err := tls.X509KeyPair(nil, nil); err == nil {
		t.Skip()
	}
	if len(caPEM) == 0 {
		t.Error("Identity returned no CA to install")
	}
}

func TestIdentityRepairsAnUnusableBundle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Identity(path); err != nil {
		t.Errorf("a corrupt bundle left the relay with no identity: %v", err)
	}
}

// The subject never changes, so neither does the filename. uninstall.py names
// it as a literal rather than keeping a file to remember it by.
func TestEveryMintedAuthorityLandsOnTheSameName(t *testing.T) {
	const want = "4c55d173.0"
	for range 2 {
		_, caPEM, _, err := mint()
		if err != nil {
			t.Fatal(err)
		}
		got, err := TrustName(caPEM)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("a minted authority is trusted as %s, want %s: deploy/uninstall.py "+
				"removes that path by name and would leave this one behind", got, want)
		}
	}
}
