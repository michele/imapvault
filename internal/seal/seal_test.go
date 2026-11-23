package seal

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
)

func newKey(t *testing.T) (recipient string, identityText string) {
	t.Helper()
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id.Recipient(), IdentityFileText(id)
}

func sealerFor(t *testing.T, recipients ...string) *Sealer {
	t.Helper()
	rs, err := ParseRecipients(recipients)
	if err != nil {
		t.Fatal(err)
	}
	return NewSealer(rs)
}

func openerFor(t *testing.T, identityTexts ...string) *Opener {
	t.Helper()
	ids, err := ParseIdentities(strings.Join(identityTexts, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return NewOpener(ids)
}

func TestGenerateIdentityRoundTripsThroughFileText(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	text := IdentityFileText(id)
	for _, want := range []string{"# created: ", "# public key: " + id.Recipient(), "AGE-SECRET-KEY-1"} {
		if !strings.Contains(text, want) {
			t.Errorf("identity file lacks %q:\n%s", want, text)
		}
	}
	if !strings.HasSuffix(text, "\n") {
		t.Error("identity file must end with a newline")
	}
	ids, err := ParseIdentities(text)
	if err != nil || len(ids) != 1 {
		t.Fatalf("parse back: %v %v", ids, err)
	}
	if _, err := ParseRecipients([]string{id.Recipient()}); err != nil {
		t.Fatalf("generated recipient does not parse: %v", err)
	}
}

func TestParseRecipientsErrors(t *testing.T) {
	_, id := newKey(t)
	secret := id[strings.Index(id, "AGE-SECRET-KEY-1"):]
	cases := map[string][]string{
		"garbage":           {"not-a-key"},
		"empty":             {""},
		"a private key":     {strings.TrimSpace(secret)},
		"two on one string": {"age1aaa\nage1bbb"},
	}
	for name, in := range cases {
		_, err := ParseRecipients(in)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: error leaks key material: %v", name, err)
		}
	}
	_, err := ParseRecipients([]string{strings.TrimSpace(secret)})
	if err == nil || !strings.Contains(err.Error(), "private key") {
		t.Errorf("pasting a private key should say so: %v", err)
	}
}

func TestParseIdentitiesErrorsNeverLeak(t *testing.T) {
	for _, in := range []string{"", "# only a comment\n", "AGE-SECRET-KEY-1NOTVALIDNOTVALID\n", "hunter2\n"} {
		_, err := ParseIdentities(in)
		if err == nil {
			t.Errorf("%q accepted", in)
			continue
		}
		if strings.Contains(err.Error(), "NOTVALID") || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("error leaks input: %v", err)
		}
	}
}

func TestSealOpenRoundTripWithTwoRecipients(t *testing.T) {
	r1, id1 := newKey(t)
	r2, id2 := newKey(t)
	s := sealerFor(t, r1, r2)
	msg := []byte("From: someone@example.com")
	sealed, err := s.Seal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if sealed[0] != prefixAge || bytes.Contains(sealed, msg) {
		t.Fatal("not encrypted")
	}
	for _, id := range []string{id1, id2} {
		got, err := openerFor(t, id).Open(sealed)
		if err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("open: %v", err)
		}
	}
	// a holder of both keys also works, as does an extra unrelated key.
	_, other := newKey(t)
	if got, err := openerFor(t, other, id2).Open(sealed); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("open with several identities: %v", err)
	}
}

func TestOpenWithWrongOrNoIdentity(t *testing.T) {
	r1, _ := newKey(t)
	_, stranger := newKey(t)
	sealed, _ := sealerFor(t, r1).Seal([]byte("secret"))
	if _, err := openerFor(t, stranger).Open(sealed); !errors.Is(err, ErrWrongIdentity) {
		t.Fatalf("want ErrWrongIdentity, got %v", err)
	}
	if _, err := NewOpener(nil).Open(sealed); !errors.Is(err, ErrNeedIdentity) {
		t.Fatalf("want ErrNeedIdentity, got %v", err)
	}
}

func TestPlainModeRoundTripNeedsNoKey(t *testing.T) {
	s := NewSealer(nil)
	if s.Encrypting() {
		t.Fatal("no recipients must mean plain mode")
	}
	sealed, err := s.Seal([]byte("visible"))
	if err != nil || sealed[0] != prefixPlain {
		t.Fatalf("seal: %v %v", sealed, err)
	}
	got, err := NewOpener(nil).Open(sealed)
	if err != nil || string(got) != "visible" {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := s.Encrypt([]byte("x")); err == nil {
		t.Fatal("raw Encrypt must refuse in plain mode")
	}
}

func TestOpenRejectsMalformedInput(t *testing.T) {
	r, id := newKey(t)
	sealed, _ := sealerFor(t, r).Seal([]byte("payload payload payload"))
	o := openerFor(t, id)
	for name, in := range map[string][]byte{"empty": {}, "unknown prefix": {0x7f, 1, 2}, "prefix only": {prefixAge}} {
		if _, err := o.Open(in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// every truncation and every single-byte flip must fail; never return wrong data.
	for n := 0; n < len(sealed); n++ {
		if got, err := o.Open(sealed[:n]); err == nil {
			t.Fatalf("truncation to %d accepted (%q)", n, got)
		}
	}
	for i := 1; i < len(sealed); i++ { // byte 0 is the prefix, covered above
		bad := append([]byte(nil), sealed...)
		bad[i] ^= 0x01
		if got, err := o.Open(bad); err == nil {
			t.Fatalf("flip at byte %d accepted (%q)", i, got)
		}
	}
}

func TestSealIsRandomised(t *testing.T) {
	r, _ := newKey(t)
	s := sealerFor(t, r)
	a, _ := s.Seal([]byte("same"))
	b, _ := s.Seal([]byte("same"))
	if bytes.Equal(a, b) {
		t.Fatal("age output must differ per call; dedupe has to key on the plaintext hash")
	}
}

func TestEncryptDecryptRawLargeAndEmpty(t *testing.T) {
	r, id := newKey(t)
	s, o := sealerFor(t, r), openerFor(t, id)
	big := make([]byte, 3<<20)
	_, _ = rand.Read(big)
	for _, plain := range [][]byte{{}, {0}, big} {
		ct, err := s.Encrypt(plain)
		if err != nil {
			t.Fatal(err)
		}
		got, err := o.Decrypt(ct)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("len %d: %v", len(plain), err)
		}
	}
}

func TestFolderHash(t *testing.T) {
	k1, err := NewFolderKey()
	if err != nil || len(k1) != 32 {
		t.Fatalf("key: %d %v", len(k1), err)
	}
	k2, _ := NewFolderKey()
	if bytes.Equal(k1, k2) {
		t.Fatal("keys must be random")
	}
	first, again := FolderHash(k1, "INBOX"), FolderHash(k1, "INBOX")
	if first != again {
		t.Fatal("must be deterministic")
	}
	if FolderHash(k1, "INBOX") == FolderHash(k1, "Inbox") || FolderHash(k1, "INBOX") == FolderHash(k2, "INBOX") {
		t.Fatal("name and key must both matter")
	}
	if FolderHash(k1, "INBOX") == sha256.Sum256([]byte("INBOX")) {
		t.Fatal("must be keyed, not a bare hash")
	}
}

func TestCanary(t *testing.T) {
	r, id := newKey(t)
	_, stranger := newKey(t)
	sealed, hash, err := NewCanary(sealerFor(t, r))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckCanary(openerFor(t, id), sealed, hash); err != nil {
		t.Fatalf("right key: %v", err)
	}
	if err := CheckCanary(openerFor(t, stranger), sealed, hash); !errors.Is(err, ErrWrongIdentity) {
		t.Fatalf("wrong key: want ErrWrongIdentity, got %v", err)
	}
	if err := CheckCanary(NewOpener(nil), sealed, hash); !errors.Is(err, ErrNeedIdentity) {
		t.Fatalf("no key: want ErrNeedIdentity, got %v", err)
	}
	hash[0] ^= 1
	if err := CheckCanary(openerFor(t, id), sealed, hash); err == nil {
		t.Fatal("a canary that decrypts to the wrong value must fail")
	}
}
