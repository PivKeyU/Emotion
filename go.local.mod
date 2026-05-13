// This is a SANDBOX-ONLY overlay. To build inside the Kiro sandbox where the
// public Go module proxy is unreachable, run:
//
//   cp go.local.mod go.mod
//   go build ./...
//
// Normal users should ignore this file and use the checked-in go.mod, which
// pulls modules from proxy.golang.org as usual.
module github.com/PivKeyU/Next-Emby

go 1.25.0

require (
	github.com/go-chi/chi/v5 v5.0.0
	github.com/go-sql-driver/mysql v1.8.1
	github.com/joho/godotenv v1.5.1
	golang.org/x/crypto v0.28.0
)

require filippo.io/edwards25519 v1.2.0 // indirect

replace (
	filippo.io/edwards25519 => ../edwards25519
	github.com/go-chi/chi/v5 => ../chi
	github.com/go-sql-driver/mysql => ../mysql
	github.com/joho/godotenv => ../godotenv
	golang.org/x/crypto => ../crypto
)
