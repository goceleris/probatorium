// Package vulnctl is a NEGATIVE CONTROL for the govulncheck job and is
// removed by the next commit: it calls a symbol with a known advisory
// (GO-2020-0017, github.com/dgrijalva/jwt-go, no fixed version).
package vulnctl

import jwt "github.com/dgrijalva/jwt-go"

// Check reaches the vulnerable VerifyAudience.
func Check(aud string) bool { return jwt.MapClaims{"aud": aud}.VerifyAudience("x", true) }
