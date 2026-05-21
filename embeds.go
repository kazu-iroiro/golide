package main

import (
	"embed"
)

//go:embed dashboard.html golide.svg MPLUS1p-Bold.ttf FontsLicense.md README.md
var embedFS embed.FS
