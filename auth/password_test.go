package auth

import (
	"strings"
	"testing"
)

func TestHashPassword(t *testing.T) {
	password := []byte(" password with spaces ")
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateHash(hash); err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(hash, password) || VerifyPassword(hash, []byte("wrong")) {
		t.Fatal("password verification did not distinguish correct and wrong passwords")
	}
	second, err := HashPassword(password)
	if err != nil || second == hash {
		t.Fatal("resetting the same password must generate a different hash")
	}
	for _, password := range [][]byte{nil, []byte(strings.Repeat("x", MaxPasswordBytes+1))} {
		if _, err := HashPassword(password); err == nil {
			t.Fatal("invalid password length was accepted")
		}
		if VerifyPassword(hash, password) {
			t.Fatal("invalid password length verified")
		}
	}
}

func TestValidateHash(t *testing.T) {
	for _, hash := range []string{
		"", "$argon2id$v=19$m=999999999,t=3,p=2$x$y", hashPrefix,
		hashPrefix + "bad$base64", hashPrefix + "a$b$c",
	} {
		t.Run(hash, func(t *testing.T) {
			if ValidateHash(hash) == nil || VerifyPassword(hash, []byte("password")) {
				t.Fatal("malformed or unbounded hash was accepted")
			}
		})
	}
}
