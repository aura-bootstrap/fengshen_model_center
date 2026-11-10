package ui

import "embed"

//go:embed index.html styles.css app.js
var Assets embed.FS
