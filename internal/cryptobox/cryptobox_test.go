package cryptobox

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"testing"
)

func newTestBox(t *testing.T) *Box {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	b, err := New(key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func TestRoundtrip(t *testing.T) {
	b := newTestBox(t)
	cases := []string{"", "hello", strings.Repeat("x", 4096), "ünïçødé 🔐"}
	for _, in := range cases {
		ct, err := b.Seal(in)
		if err != nil {
			t.Fatalf("Seal(%q): %v", in, err)
		}
		out, err := b.Open(ct)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if out != in {
			t.Fatalf("roundtrip: got %q want %q", out, in)
		}
	}
}

func TestEmptyRoundtripsAsNil(t *testing.T) {
	b := newTestBox(t)
	ct, err := b.Seal("")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if ct != nil {
		t.Fatalf("Seal(\"\") should be nil, got %v", ct)
	}
	out, err := b.Open(nil)
	if err != nil {
		t.Fatalf("Open(nil): %v", err)
	}
	if out != "" {
		t.Fatalf("Open(nil) = %q, want empty", out)
	}
}

func TestShortCiphertext(t *testing.T) {
	b := newTestBox(t)
	if _, err := b.Open([]byte{0x01, 0x02}); err == nil {
		t.Fatal("expected error for ciphertext shorter than nonce size")
	}
}

func TestTamperedCiphertextFails(t *testing.T) {
	b := newTestBox(t)
	ct, _ := b.Seal("secret")
	ct[len(ct)-1] ^= 0xff
	if _, err := b.Open(ct); err == nil {
		t.Fatal("expected GCM auth failure on tampered ciphertext")
	}
}

func TestWrongKeyLength(t *testing.T) {
	if _, err := New(make([]byte, 16)); err == nil {
		t.Fatal("expected error for 16-byte key")
	}
	if _, err := New(make([]byte, 0)); err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestNewFromHex(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	b, err := NewFromHex(hex.EncodeToString(key))
	if err != nil {
		t.Fatalf("NewFromHex: %v", err)
	}
	ct, err := b.Seal("v")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Equal(ct, []byte("v")) {
		t.Fatal("ciphertext equals plaintext")
	}
	if _, err := NewFromHex("not-hex"); err == nil {
		t.Fatal("expected error for non-hex key")
	}
}

func TestConcurrent(t *testing.T) {
	b := newTestBox(t)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			plain := strings.Repeat("z", i%17)
			ct, err := b.Seal(plain)
			if err != nil {
				t.Errorf("seal: %v", err)
				return
			}
			got, err := b.Open(ct)
			if err != nil {
				t.Errorf("open: %v", err)
				return
			}
			if got != plain {
				t.Errorf("got %q want %q", got, plain)
			}
		}(i)
	}
	wg.Wait()
}
