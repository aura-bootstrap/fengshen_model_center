package runtimeadapter

import "syscall"

func llamaCPUID(leaf, subleaf uint32) (eax, ebx, ecx, edx uint32)
func llamaXGETBV() uint64

func llamaHostAVX2() bool {
	feature := syscall.NewLazyDLL("kernel32.dll").NewProc("IsProcessorFeaturePresent")
	avx, _, _ := feature.Call(39)
	avx2, _, _ := feature.Call(40)
	if avx == 0 || avx2 == 0 {
		return false
	}
	max, _, _, _ := llamaCPUID(0, 0)
	if max < 7 {
		return false
	}
	_, _, ecx, _ := llamaCPUID(1, 0)
	// XGETBV is legal only after XSAVE and OSXSAVE are established.
	if ecx&(1<<26|1<<27) != 1<<26|1<<27 {
		return false
	}
	_, ebx, _, _ := llamaCPUID(7, 0)
	// SSE4.2 remains enabled in both profiles.
	return ecx&(1<<20) != 0 && llamaAVX2Registers(ecx, ebx, llamaXGETBV(), true, true)
}
