#include "textflag.h"

// func findHeaderEnd(buf []byte) int
TEXT ·findHeaderEnd(SB), NOSPLIT, $0-32
	MOVD buf_base+0(FP), R0
	MOVD buf_len+8(FP), R1
	CMP  $4, R1
	BLT  not_found

	MOVD $0x0D0D0D0D0D0D0D0D, R2
	VDUP R2, V0.D2

	MOVD $0, R3

	MOVD R1, R4
	SUB  $19, R4, R4

	CMP  R4, R3
	BGT  scalar_init

simd_loop:
	ADD  R0, R3, R6
	VLD1 (R6), [V1.B16]
	VCMEQ V0.B16, V1.B16, V2.B16
	VUADDLV V2.B16, V3
	VMOV V3.D[0], R5
	CBZ  R5, simd_next

	MOVD R3, R7
	ADD  $16, R3, R8
	SUB  $3, R1, R9
	CMP  R9, R8
	BLE  block_scan
	MOVD R9, R8

block_scan:
	CMP  R8, R7
	BGE  simd_next_from_r3
	ADD  R0, R7, R6
	MOVWU (R6), R10
	MOVD $0x0A0D0A0D, R11
	CMP  R11, R10
	BEQ  found_at_r7
	ADD  $1, R7
	B    block_scan

simd_next_from_r3:
	ADD  $16, R3
	CMP  R4, R3
	BLE  simd_loop
	B    scalar_init

found_at_r7:
	ADD  $4, R7
	MOVD R7, ret+24(FP)
	RET

simd_next:
	ADD  $16, R3
	CMP  R4, R3
	BLE  simd_loop

scalar_init:
	SUB  $3, R1, R4

scalar_loop:
	CMP  R4, R3
	BGE  not_found
	ADD  R0, R3, R6
	MOVWU (R6), R5
	MOVD $0x0A0D0A0D, R7
	CMP  R7, R5
	BEQ  scalar_found
	ADD  $1, R3
	B    scalar_loop

scalar_found:
	ADD  $4, R3
	MOVD R3, ret+24(FP)
	RET

not_found:
	MOVD $-1, R0
	MOVD R0, ret+24(FP)
	RET
