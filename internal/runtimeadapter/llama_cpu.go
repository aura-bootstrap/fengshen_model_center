package runtimeadapter

func llamaVersionCompatible(version string, avx2 bool) bool {
	switch version {
	case "b12182-fengshen1-cpu":
		return true
	case "b12182-fengshen2-cpu-avx2", "b12182-fengshen3-vulkan":
		return avx2
	default:
		return false
	}
}

func llamaVersionDevice(version string) string {
	switch version {
	case "b12182-fengshen1-cpu", "b12182-fengshen2-cpu-avx2":
		return "cpu"
	case "b12182-fengshen3-vulkan":
		return "vulkan"
	default:
		return ""
	}
}

func llamaAVX2Registers(leaf1ECX, leaf7EBX uint32, xcr0 uint64, osAVX, osAVX2 bool) bool {
	const required1 = uint32(1<<12 | 1<<20 | 1<<26 | 1<<27 | 1<<28 | 1<<29) // FMA, SSE4.2, XSAVE, OSXSAVE, AVX, F16C
	const required7 = uint32(1<<5 | 1<<8)                           // AVX2, BMI2
	return osAVX && osAVX2 && leaf1ECX&required1 == required1 && leaf7EBX&required7 == required7 && xcr0&6 == 6
}
