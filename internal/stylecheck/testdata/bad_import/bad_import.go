package util // want "PKGNAME"

import (
	. "fmt" // want "IMPORT"
	_ "net/http/pprof" // want "IMPORT"
)

func Print(s string) {
	Println(s)
}
