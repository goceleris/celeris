#include "textflag.h"

// func findHeaderEnd(buf []byte) int
TEXT ·findHeaderEnd(SB), NOSPLIT, $0-32
	MOVQ buf_base+0(FP), SI
	MOVQ buf_len+8(FP), CX
	CMPQ CX, $4
	JL   not_found

	// Broadcast '\r' (0x0D) into X0
	MOVQ $0x0D0D0D0D0D0D0D0D, AX
	MOVQ AX, X0
	PUNPCKLQDQ X0, X0

	XORQ DI, DI              // DI = current offset

	MOVQ CX, DX
	SUBQ $19, DX              // last safe SIMD start = len - 16 - 3
	JL   scalar_init

simd_loop:
	CMPQ DI, DX
	JG   scalar_init
	MOVOU (SI)(DI*1), X1
	PCMPEQB X0, X1
	PMOVMSKB X1, AX
	TESTL AX, AX
	JZ   simd_next

check_bits:
	BSFL  AX, BX              // BX = bit index of first \r
	LEAQ  (DI)(BX*1), R8      // R8 = absolute position
	LEAQ  4(R8), R9
	CMPQ  R9, CX
	JG    clear_and_continue
	MOVL  (SI)(R8*1), R10
	CMPL  R10, $0x0A0D0A0D    // 0x0A0D0A0D = \r\n\r\n in little-endian byte order
	JE    found_at_r8
	BTRL  BX, AX
	TESTL AX, AX
	JNZ   check_bits

simd_next:
	ADDQ $16, DI
	JMP  simd_loop

clear_and_continue:
	BTRL BX, AX
	TESTL AX, AX
	JNZ  check_bits
	ADDQ $16, DI
	JMP  simd_loop

found_at_r8:
	LEAQ 4(R8), AX
	MOVQ AX, ret+24(FP)
	RET

scalar_init:
	MOVQ CX, DX
	SUBQ $3, DX

scalar_loop:
	CMPQ DI, DX
	JGE  not_found
	MOVL (SI)(DI*1), AX
	CMPL AX, $0x0A0D0A0D      // 0x0A0D0A0D = \r\n\r\n in little-endian byte order
	JE   scalar_found
	INCQ DI
	JMP  scalar_loop

scalar_found:
	LEAQ 4(DI), AX
	MOVQ AX, ret+24(FP)
	RET

not_found:
	MOVQ $-1, ret+24(FP)
	RET
