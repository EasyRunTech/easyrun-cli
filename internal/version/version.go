// Package version — versi program, diisi saat build lewat -ldflags.
package version

// Version diisi workflow rilis (`-X …/version.Version=v0.1.0`). Build lokal
// tanpa ldflags menampilkan "dev".
var Version = "dev"
