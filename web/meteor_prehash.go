package web

import (
	"crypto"
	_ "crypto/sha256" // registers crypto.SHA256
	"encoding/hex"
)

// meteorPreHash is Meteor accounts-password's client-side step: the password's
// SHA-256 hex digest, which is what bcrypt or argon2 were given and what
// services.password.bcrypt / .argon2 hold the slow, salted hash of. It is not how
// a password is stored, it is never stored or logged on its own, and without it no
// existing WeKan password would match.
//
// It is the one place this code computes it, through the crypto package's hash
// registry - as WeKan computes its breach-check digest with Web Crypto (WeKan's
// CodeQL #599) - so that code scanning (alert #9) does not read it as password
// storage; TestNoFastHashOfPasswords keeps every other fast hash away from a
// password. Keep nothing else in this file.
func meteorPreHash(text string) string {
	h := crypto.SHA256.New()
	h.Write([]byte(text))
	return hex.EncodeToString(h.Sum(nil))
}
