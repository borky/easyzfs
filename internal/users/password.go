// password.go — hash de contraseñas con argon2id (formato PHC).
package users

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Parámetros argon2id (equilibrio seguridad/huella en un LXC modesto).
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16
)

// argonSlot — one argon2 computation at a time. Each one allocates its
// whole memory cost (64 MiB) at once, and the unit caps the service at
// MemoryMax=256M: four logins or re-authentications in parallel — the
// login endpoint needs no session, and its rate limit is per address — got
// the service OOM-killed on the Proxmox test VM (79 → 246 MB in half a
// second). Serialised, a burst only waits.
var argonSlot = make(chan struct{}, 1)

// maxArgonMemory — the largest memory cost a stored hash may ask for (KiB).
// Hashes can arrive from a backup import; one claiming gigabytes must not
// allocate them.
const maxArgonMemory = 2 * argonMemory

func argonKey(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
	argonSlot <- struct{}{}
	defer func() { <-argonSlot }()
	return idKey(password, salt, time, memory, threads, keyLen)
}

// idKey — test seam.
var idKey = argon2.IDKey

// hashPassword genera el hash PHC: $argon2id$v=19$m=...,t=...,p=...$salt$hash
func hashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argonKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// verifyPassword compara en tiempo constante contra el hash PHC almacenado.
func verifyPassword(password, phc string) bool {
	parts := strings.Split(phc, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var memory, time, threads uint32
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false
	}
	if memory > maxArgonMemory || time > 16 || threads == 0 || threads > 16 {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil {
		return false
	}
	key := argonKey([]byte(password), salt, time, memory, uint8(threads), uint32(len(want)))
	return subtle.ConstantTimeCompare(key, want) == 1
}
