#include "textflag.h"

// func findHeaderEnd(buf []byte) int
//
// Scans buf for the HTTP header terminator \r\n\r\n (0x0D 0x0A 0x0D 0x0A).
// Returns the index one past the terminator (i.e., the start of the body),
// or -1 if not found.
//
// At entry the function checks the package-level useAVX2 flag and dispatches
// to either a 32-byte (AVX2) or 16-byte (SSE2) vectorized loop. Both fall
// through to the same scalar tail.
TEXT ·findHeaderEnd(SB), NOSPLIT, $0-32
	MOVQ buf_base+0(FP), SI   // SI = &buf[0]
	MOVQ buf_len+8(FP), CX    // CX = len(buf)
	CMPQ CX, $4
	JL   not_found

	XORQ DI, DI               // DI = current offset

	// Check runtime AVX2 flag.
	CMPB ·useAVX2(SB), $0
	JE   sse2_init

// -----------------------------------------------------------------------
// AVX2 path — 32 bytes per iteration
// -----------------------------------------------------------------------
avx2_init:
	// Broadcast '\r' (0x0D) into Y0 (32 bytes).
	MOVQ    $0x0D0D0D0D0D0D0D0D, AX
	MOVQ    AX, X0
	VPBROADCASTQ X0, Y0

	MOVQ CX, DX
	SUBQ $35, DX              // last safe AVX2 start = len - 32 - 3
	JL   avx2_to_sse2         // buffer too small for even one AVX2 iteration

avx2_loop:
	CMPQ DI, DX
	JG   avx2_to_sse2

	// Load 32 unaligned bytes, compare each byte against '\r'.
	VMOVDQU (SI)(DI*1), Y1
	VPCMPEQB Y0, Y1, Y2
	VPMOVMSKB Y2, AX          // AX = 32-bit mask of '\r' positions
	TESTL AX, AX
	JZ    avx2_next

avx2_check_bits:
	BSFL  AX, BX              // BX = index of first set bit
	LEAQ  (DI)(BX*1), R8      // R8 = absolute position in buf
	LEAQ  4(R8), R9
	CMPQ  R9, CX
	JG    avx2_clear_bit
	MOVL  (SI)(R8*1), R10
	CMPL  R10, $0x0A0D0A0D    // \r\n\r\n in little-endian
	JE    avx2_found
	BTRL  BX, AX              // clear this bit, check next '\r'
	TESTL AX, AX
	JNZ   avx2_check_bits

avx2_next:
	ADDQ $32, DI
	JMP  avx2_loop

avx2_clear_bit:
	BTRL BX, AX
	TESTL AX, AX
	JNZ  avx2_check_bits
	ADDQ $32, DI
	JMP  avx2_loop

avx2_found:
	VZEROUPPER
	LEAQ 4(R8), AX
	MOVQ AX, ret+24(FP)
	RET

	// Transition: remaining bytes too few for 32-byte loads.
	// Fall through to SSE2 for 16-byte chunks, then scalar tail.
avx2_to_sse2:
	VZEROUPPER

// -----------------------------------------------------------------------
// SSE2 path — 16 bytes per iteration
// -----------------------------------------------------------------------
sse2_init:
	// Broadcast '\r' (0x0D) into X0 (16 bytes).
	MOVQ $0x0D0D0D0D0D0D0D0D, AX
	MOVQ AX, X0
	PUNPCKLQDQ X0, X0

	MOVQ CX, DX
	SUBQ $19, DX              // last safe SSE2 start = len - 16 - 3
	JL   scalar_init

sse2_loop:
	CMPQ DI, DX
	JG   scalar_init

	MOVOU (SI)(DI*1), X1
	PCMPEQB X0, X1
	PMOVMSKB X1, AX
	TESTL AX, AX
	JZ   sse2_next

sse2_check_bits:
	BSFL  AX, BX
	LEAQ  (DI)(BX*1), R8
	LEAQ  4(R8), R9
	CMPQ  R9, CX
	JG    sse2_clear_bit
	MOVL  (SI)(R8*1), R10
	CMPL  R10, $0x0A0D0A0D
	JE    found_at_r8
	BTRL  BX, AX
	TESTL AX, AX
	JNZ   sse2_check_bits

sse2_next:
	ADDQ $16, DI
	JMP  sse2_loop

sse2_clear_bit:
	BTRL BX, AX
	TESTL AX, AX
	JNZ  sse2_check_bits
	ADDQ $16, DI
	JMP  sse2_loop

// -----------------------------------------------------------------------
// Scalar tail — one byte at a time for remaining bytes
// -----------------------------------------------------------------------
scalar_init:
	MOVQ CX, DX
	SUBQ $3, DX

scalar_loop:
	CMPQ DI, DX
	JGE  not_found
	MOVL (SI)(DI*1), AX
	CMPL AX, $0x0A0D0A0D
	JE   scalar_found
	INCQ DI
	JMP  scalar_loop

scalar_found:
	LEAQ 4(DI), AX
	MOVQ AX, ret+24(FP)
	RET

found_at_r8:
	LEAQ 4(R8), AX
	MOVQ AX, ret+24(FP)
	RET

not_found:
	MOVQ $-1, ret+24(FP)
	RET
