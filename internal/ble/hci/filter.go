package hci

// Monitor channel packets start with a header of three little-endian 16-bit fields: opcode, controller index
// and payload length. Opcode 3 carries an HCI event: event code, parameter length, parameters.
const (
	monHdrLen  = 6
	monEventPk = 3
)

// insn is a classic BPF instruction, laid out like the kernel's struct sock_filter.
type insn struct {
	code   uint16
	jt, jf uint8
	k      uint32
}

const (
	bpfLdAbsB = 0x30 // BPF_LD | BPF_B | BPF_ABS: A = packet[k]
	bpfJeqK   = 0x15 // BPF_JMP | BPF_JEQ | BPF_K: if A == k skip jt, else skip jf
	bpfRetK   = 0x06 // BPF_RET | BPF_K: accept k bytes (0 drops the packet)
)

// filterProg is a socket filter that passes only LE advertising report events of one controller. The monitor
// channel copies every HCI packet of every controller to each reader, ACL data of active connections
// included; the filter drops the rest in the kernel, so the proxy wakes up only for what it uses.
func filterProg(index uint16) []insn {
	byteIs := func(off, v uint32, rejectAt int, at int) []insn {
		return []insn{{bpfLdAbsB, 0, 0, off}, {bpfJeqK, 0, uint8(rejectAt - at - 2), v}}
	}
	const reject = 5*2 + 4 // index of the final "ret 0": five byte checks, then four instructions
	var p []insn
	for _, c := range [][2]uint32{
		{0, monEventPk}, {1, 0}, // opcode
		{2, uint32(index & 0xff)}, {3, uint32(index >> 8)},
		{monHdrLen, evtLEMeta},
	} {
		p = append(p, byteIs(c[0], c[1], reject, len(p))...)
	}
	return append(p,
		insn{bpfLdAbsB, 0, 0, monHdrLen + 2}, // LE Meta subevent code
		insn{bpfJeqK, 1, 0, subLEAdvReport},
		insn{bpfJeqK, 0, 1, subLEExtAdvReport},
		insn{bpfRetK, 0, 0, 0xffff},
		insn{bpfRetK, 0, 0, 0},
	)
}
