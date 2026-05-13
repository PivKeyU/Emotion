// This is a SANDBOX-ONLY overlay. To build inside a sandbox where the public Go
// module proxy is unreachable, copy this file to go.mod and point the replace
// paths at local module mirrors.
//
// Normal users should ignore this file and use the checked-in go.mod, which
// pulls modules from proxy.golang.org as usual.
module github.com/PivKeyU/Emotion

go 1.24

require (
	github.com/go-chi/chi/v5 v5.1.0
	github.com/jackc/pgx/v5 v5.7.5
	github.com/joho/godotenv v1.5.1
	golang.org/x/crypto v0.37.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.13.0 // indirect
	golang.org/x/text v0.24.0 // indirect
)

replace (
	github.com/go-chi/chi/v5 => ../chi
	github.com/jackc/pgpassfile => ../pgpassfile
	github.com/jackc/pgservicefile => ../pgservicefile
	github.com/jackc/pgx/v5 => ../pgx
	github.com/jackc/puddle/v2 => ../puddle
	github.com/joho/godotenv => ../godotenv
	golang.org/x/crypto => ../crypto
	golang.org/x/sync => ../sync
	golang.org/x/text => ../text
)