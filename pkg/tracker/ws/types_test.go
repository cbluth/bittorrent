package ws

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestBinaryStringJSON(t *testing.T) {
	original := binaryString{0x00, 0x7f, 0x80, 0xff}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded binaryString
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, original) {
		t.Fatalf("binary string round trip: got %x, want %x", decoded, original)
	}
	if len([]rune(binaryText(original))) != 4 {
		t.Fatal("offer ID byte length changed")
	}
}

func TestBinaryStringRejectsNonByteCharacters(t *testing.T) {
	var decoded binaryString
	if err := json.Unmarshal([]byte("\"\\u0100\""), &decoded); err == nil {
		t.Fatal("expected invalid binary string to fail")
	}
}
