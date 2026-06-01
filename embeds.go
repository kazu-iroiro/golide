package main

import (
	"embed"
)

//go:embed dashboard.html golide.svg MPLUS1p-Bold.ttf NotoEmoji-VariableFont_wght.ttf FontsLicense.md README.md
var embedFS embed.FS
