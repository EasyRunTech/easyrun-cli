module github.com/EasyRunTech/easyrun-cli

go 1.25.0

require (
	github.com/EasyRunTech/easyrun-tunnel-agent v0.1.0
	github.com/gorilla/websocket v1.5.3
	github.com/hashicorp/yamux v0.1.2
	golang.org/x/crypto v0.55.0
)

require golang.org/x/sys v0.47.0

// TODO(F2 tunnel): hapus replace ini sebelum push — modul agen belum terbit
// (repo publik EasyRunTech/easyrun-tunnel-agent + tag v0.1.0). Selama replace
// masih ada, CI dan `go install` di luar laptop ini gagal.
replace github.com/EasyRunTech/easyrun-tunnel-agent => ../easyrun-tunnel-agent
