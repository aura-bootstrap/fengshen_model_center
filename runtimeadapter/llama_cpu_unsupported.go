//go:build !windows || !amd64

package runtimeadapter

func llamaHostAVX2() bool { return false }
