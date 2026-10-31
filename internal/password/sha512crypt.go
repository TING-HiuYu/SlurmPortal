package password

import (
	"crypto/sha512"
	"errors"
	"strconv"
	"strings"
)

// SHA-512 crypt per Ulrich Drepper's specification ("$6$"), the format OpenLDAP {CRYPT} and glibc verify.

const (
	cryptAlphabet      = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	cryptDefaultRounds = 5000
	cryptMinRounds     = 1000
	cryptMaxRounds     = 999999999
	cryptMaxSalt       = 16
)

func repeatTo(src []byte, n int) []byte {
	out := make([]byte, 0, n)
	for len(out)+len(src) <= n {
		out = append(out, src...)
	}
	return append(out, src[:n-len(out)]...)
}

func sha512Crypt(pw, salt []byte, rounds int) [64]byte {
	b := sha512.New()
	b.Write(pw)
	b.Write(salt)
	b.Write(pw)
	bSum := b.Sum(nil)

	a := sha512.New()
	a.Write(pw)
	a.Write(salt)
	a.Write(repeatTo(bSum, len(pw)))
	for i := len(pw); i > 0; i >>= 1 {
		if i&1 == 1 {
			a.Write(bSum)
		} else {
			a.Write(pw)
		}
	}
	aSum := a.Sum(nil)

	dp := sha512.New()
	for range pw {
		dp.Write(pw)
	}
	p := repeatTo(dp.Sum(nil), len(pw))

	ds := sha512.New()
	for range 16 + int(aSum[0]) {
		ds.Write(salt)
	}
	s := repeatTo(ds.Sum(nil), len(salt))

	c := aSum
	for i := range rounds {
		h := sha512.New()
		if i&1 == 1 {
			h.Write(p)
		} else {
			h.Write(c)
		}
		if i%3 != 0 {
			h.Write(s)
		}
		if i%7 != 0 {
			h.Write(p)
		}
		if i&1 == 1 {
			h.Write(c)
		} else {
			h.Write(p)
		}
		c = h.Sum(c[:0])
	}
	var out [64]byte
	copy(out[:], c)
	return out
}

var cryptOrder = [21][3]int{
	{0, 21, 42}, {22, 43, 1}, {44, 2, 23}, {3, 24, 45}, {25, 46, 4}, {47, 5, 26}, {6, 27, 48},
	{28, 49, 7}, {50, 8, 29}, {9, 30, 51}, {31, 52, 10}, {53, 11, 32}, {12, 33, 54}, {34, 55, 13},
	{56, 14, 35}, {15, 36, 57}, {37, 58, 16}, {59, 17, 38}, {18, 39, 60}, {40, 61, 19}, {62, 20, 41},
}

func cryptEncode(sum [64]byte) string {
	var sb strings.Builder
	put := func(b2, b1, b0 byte, n int) {
		w := uint(b2)<<16 | uint(b1)<<8 | uint(b0)
		for range n {
			sb.WriteByte(cryptAlphabet[w&0x3f])
			w >>= 6
		}
	}
	for _, g := range cryptOrder {
		put(sum[g[0]], sum[g[1]], sum[g[2]], 4)
	}
	put(0, 0, sum[63], 2)
	return sb.String()
}

// CryptSHA512 formats a "$6$" hash. rounds==0 uses the implicit default and omits the rounds field.
func CryptSHA512(pw []byte, salt string, rounds int) (string, error) {
	if strings.ContainsAny(salt, "$:\n") {
		return "", errors.New("invalid crypt salt")
	}
	if len(salt) > cryptMaxSalt {
		salt = salt[:cryptMaxSalt]
	}
	prefix := "$6$"
	if rounds != 0 {
		rounds = min(max(rounds, cryptMinRounds), cryptMaxRounds)
		prefix += "rounds=" + strconv.Itoa(rounds) + "$"
	} else {
		rounds = cryptDefaultRounds
	}
	return prefix + salt + "$" + cryptEncode(sha512Crypt(pw, []byte(salt), rounds)), nil
}

// parseCrypt splits "$6$[rounds=N$]salt$hash" into its settings.
func parseCrypt(h string) (salt string, rounds int, ok bool) {
	rest, ok := strings.CutPrefix(h, "$6$")
	if !ok {
		return "", 0, false
	}
	if r, after, found := strings.Cut(rest, "$"); found && strings.HasPrefix(r, "rounds=") {
		n, err := strconv.Atoi(strings.TrimPrefix(r, "rounds="))
		if err != nil || n < cryptMinRounds || n > cryptMaxRounds {
			return "", 0, false
		}
		rounds, rest = n, after
	}
	salt, _, ok = strings.Cut(rest, "$")
	return salt, rounds, ok && len(salt) <= cryptMaxSalt
}
