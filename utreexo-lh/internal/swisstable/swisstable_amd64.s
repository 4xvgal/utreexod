//go:build unix && amd64

#include "textflag.h"

// ============================================================================
// Swiss Table SIMD matching functions (SSE2 only)
// ============================================================================
//
// These functions scan 16 control bytes at once using SSE registers.
// Each returns a 16-bit mask where bit i is set if ctrl[i] matched.
//
// Only SSE2 instructions are used, which is baseline for all amd64 CPUs.
//
// Go assembly pseudo-registers:
//   FP = Frame Pointer (arguments from caller)
//   SP = Stack Pointer (local variables)
//   SB = Static Base (global symbols)
//
// XMM registers (128-bit, holds 16 bytes):
//   X0, X1, X2, ... are SSE registers
//
// ============================================================================

// ----------------------------------------------------------------------------
// func matchH2(ctrl []byte, h2 byte) uint16
// ----------------------------------------------------------------------------
// Finds slots where ctrl[i] == h2.
// Returns 16-bit mask: bit i set if ctrl[i] matches h2.
//
// Stack layout (34 bytes total):
//   +0   ctrl.ptr   (8 bytes)  - pointer to ctrl data
//   +8   ctrl.len   (8 bytes)  - slice length (unused)
//   +16  ctrl.cap   (8 bytes)  - slice capacity (unused)
//   +24  h2         (1 byte)   - the byte to match
//   +32  return     (2 bytes)  - result mask
//
TEXT ·matchH2(SB), NOSPLIT, $0-34
    // Load arguments
    MOVQ    ctrl+0(FP), AX          // AX = pointer to ctrl bytes
    MOVBLZX h2+24(FP), CX           // CX = h2 (zero-extended to 64-bit)

    // Broadcast h2 to all 16 bytes of X1 using SSE2 only:
    //   MOVD:       X1 = [h2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0]
    //   PUNPCKLBW:  X1 = [h2, h2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0]
    //   PUNPCKLWL:  X1 = [h2, h2, h2, h2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0]
    //   PSHUFD:     X1 = [h2, h2, h2, h2, h2, h2, h2, h2, h2, h2, h2, h2, h2, h2, h2, h2]
    MOVD    CX, X1
    PUNPCKLBW X1, X1
    PUNPCKLWL X1, X1
    PSHUFD  $0, X1, X1

    // Compare ctrl bytes against h2
    //   X0 = 16 ctrl bytes from memory
    //   PCMPEQB: X0[i] = (X0[i] == X1[i]) ? 0xFF : 0x00
    MOVOU   (AX), X0                        // X0 = ctrl[0:16]
    PCMPEQB X1, X0                          // X0[i] = 0xFF if match, 0x00 if not

    // Extract high bit of each byte into a 16-bit mask
    //   PMOVMSKB: result[i] = X0[i].bit7
    //   If ctrl[5] matched, X0[5] = 0xFF, so bit 5 of result = 1
    PMOVMSKB X0, AX                         // AX = 16-bit mask

    // Return
    MOVW    AX, ret+32(FP)
    RET

// ----------------------------------------------------------------------------
// func matchEmpty(ctrl []byte) uint16
// ----------------------------------------------------------------------------
// Finds empty slots (ctrl[i] == 0x00).
// Returns 16-bit mask: bit i set if ctrl[i] is empty.
//
// Stack layout (26 bytes total):
//   +0   ctrl.ptr   (8 bytes)
//   +8   ctrl.len   (8 bytes)
//   +16  ctrl.cap   (8 bytes)
//   +24  return     (2 bytes)
//
TEXT ·matchEmpty(SB), NOSPLIT, $0-26
    MOVQ    ctrl+0(FP), AX          // AX = pointer to ctrl bytes

    // Compare against zero
    //   PXOR X1, X1 sets X1 = 0 (XOR with self)
    //   PCMPEQB compares each byte against 0
    MOVOU   (AX), X0                // X0 = ctrl[0:16]
    PXOR    X1, X1                  // X1 = [0, 0, 0, ...]
    PCMPEQB X1, X0                  // X0[i] = 0xFF if ctrl[i] == 0

    // Extract mask
    PMOVMSKB X0, AX                 // AX = 16-bit mask
    MOVW    AX, ret+24(FP)
    RET

// ----------------------------------------------------------------------------
// func matchEmptyOrDeleted(ctrl []byte) uint16
// ----------------------------------------------------------------------------
// Finds empty (0x00) or deleted (0x7F) slots.
// Both have high bit = 0. Full slots have high bit = 1 (0x80 | h2).
// Returns 16-bit mask: bit i set if ctrl[i] is empty or deleted.
//
// Stack layout (26 bytes total):
//   +0   ctrl.ptr   (8 bytes)
//   +8   ctrl.len   (8 bytes)
//   +16  ctrl.cap   (8 bytes)
//   +24  return     (2 bytes)
//
TEXT ·matchEmptyOrDeleted(SB), NOSPLIT, $0-26
    MOVQ    ctrl+0(FP), AX          // AX = pointer to ctrl bytes

    // PMOVMSKB extracts the high bit of each byte.
    // Full slots have high bit = 1, empty/deleted have high bit = 0.
    // So we extract high bits, then invert to get empty/deleted mask.
    MOVOU   (AX), X0                // X0 = ctrl[0:16]
    PMOVMSKB X0, AX                 // AX[i] = ctrl[i].bit7 (1 if full)
    XORW    $0xFFFF, AX             // Invert: now 1 if empty/deleted

    MOVW    AX, ret+24(FP)
    RET
