#include "textflag.h"

TEXT ·llamaCPUID(SB), NOSPLIT, $0-24
	MOVL leaf+0(FP), AX
	MOVL subleaf+4(FP), CX
	CPUID
	MOVL AX, eax+8(FP)
	MOVL BX, ebx+12(FP)
	MOVL CX, ecx+16(FP)
	MOVL DX, edx+20(FP)
	RET

TEXT ·llamaXGETBV(SB), NOSPLIT, $0-8
	XORL CX, CX
	XGETBV
	SHLQ $32, DX
	ORQ DX, AX
	MOVQ AX, ret+0(FP)
	RET
