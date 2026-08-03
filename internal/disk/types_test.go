package disk

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"reflect"
	"testing"
)

// The Unmarshal methods hand-decode layouts that binary.Decode derives from
// the struct definitions. These tests pin them to each other, so a field
// reordered or resized in the struct cannot silently diverge from the manual
// offsets.

func randBytes(t *testing.T, n int, seed int64) []byte {
	t.Helper()

	b := make([]byte, n)
	r := rand.New(rand.NewSource(seed))
	if _, err := r.Read(b); err != nil {
		t.Fatal(err)
	}

	return b
}

func TestUnmarshalMatchesBinaryDecode(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
		// decode returns (manual, reflected) for the same input.
		decode func(b []byte) (any, any, error)
	}{
		{
			name: "InodeCompact",
			size: SizeInodeCompact,
			decode: func(b []byte) (any, any, error) {
				var manual, want InodeCompact
				manual.Unmarshal(b)
				_, err := binary.Decode(b, binary.LittleEndian, &want)

				return manual, want, err
			},
		},
		{
			name: "InodeExtended",
			size: SizeInodeExtended,
			decode: func(b []byte) (any, any, error) {
				var manual, want InodeExtended
				manual.Unmarshal(b)
				_, err := binary.Decode(b, binary.LittleEndian, &want)

				return manual, want, err
			},
		},
		{
			name: "Dirent",
			size: SizeDirent,
			decode: func(b []byte) (any, any, error) {
				var manual, want Dirent
				manual.Unmarshal(b)
				_, err := binary.Decode(b, binary.LittleEndian, &want)

				return manual, want, err
			},
		},
		{
			name: "XattrHeader",
			size: SizeXattrBodyHeader,
			decode: func(b []byte) (any, any, error) {
				var manual, want XattrHeader
				manual.Unmarshal(b)
				_, err := binary.Decode(b, binary.LittleEndian, &want)

				return manual, want, err
			},
		},
		{
			name: "XattrEntry",
			size: SizeXattrEntry,
			decode: func(b []byte) (any, any, error) {
				var manual, want XattrEntry
				manual.Unmarshal(b)
				_, err := binary.Decode(b, binary.LittleEndian, &want)

				return manual, want, err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The struct's binary size must match the declared constant, or
			// the two decoders are reading different amounts.
			if n, err := binary.Decode(make([]byte, tc.size), binary.LittleEndian, mustNew(t, tc)); err != nil {
				t.Fatalf("binary.Decode over %d bytes: %v", tc.size, err)
			} else if n != tc.size {
				t.Fatalf("struct decodes %d bytes, but the size constant says %d", n, tc.size)
			}

			inputs := [][]byte{
				make([]byte, tc.size),
				bytes.Repeat([]byte{0xFF}, tc.size),
			}
			for seed := range int64(200) {
				inputs = append(inputs, randBytes(t, tc.size, seed))
			}

			for _, in := range inputs {
				manual, want, err := tc.decode(in)
				if err != nil {
					t.Fatalf("binary.Decode(%x): %v", in, err)
				}
				if !reflect.DeepEqual(manual, want) {
					t.Fatalf("input %x\n manual: %+v\nreflect: %+v", in, manual, want)
				}
			}
		})
	}
}

// mustNew returns a fresh pointer to the zero value of the type under test.
func mustNew(t *testing.T, tc struct {
	name   string
	size   int
	decode func(b []byte) (any, any, error)
},
) any {
	t.Helper()

	switch tc.name {
	case "InodeCompact":
		return &InodeCompact{}
	case "InodeExtended":
		return &InodeExtended{}
	case "Dirent":
		return &Dirent{}
	case "XattrHeader":
		return &XattrHeader{}
	case "XattrEntry":
		return &XattrEntry{}
	}
	t.Fatalf("unknown case %q", tc.name)

	return nil
}

func BenchmarkDecodeInodeCompact(b *testing.B) {
	buf := make([]byte, SizeInodeCompact)
	b.Run("manual", func(b *testing.B) {
		var v InodeCompact
		for range b.N {
			v.Unmarshal(buf)
		}
	})
	b.Run("reflect", func(b *testing.B) {
		var v InodeCompact
		for range b.N {
			if _, err := binary.Decode(buf, binary.LittleEndian, &v); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkDecodeDirent(b *testing.B) {
	buf := make([]byte, SizeDirent)
	b.Run("manual", func(b *testing.B) {
		var v Dirent
		for range b.N {
			v.Unmarshal(buf)
		}
	})
	b.Run("reflect", func(b *testing.B) {
		var v Dirent
		for range b.N {
			if _, err := binary.Decode(buf, binary.LittleEndian, &v); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// TestSuperBlockMarshalMatchesReflection pins the hand-written codecs to what
// encoding/binary produces. They exist only to avoid reflection's cost, so any
// divergence is a bug: these bytes are the on-disk format.
func TestSuperBlockMarshalMatchesReflection(t *testing.T) {
	sb := SuperBlock{
		MagicNumber: MagicNumber, Checksum: 0x11223344, FeatureCompat: 0x55667788,
		BlkSizeBits: 12, ExtSlots: 3, RootNid: 0xABCD,
		Inos: 0x0102030405060708, BuildTime: 0x1112131415161718, BuildTimeNs: 0x21222324,
		Blocks: 0x31323334, MetaBlkAddr: 0x41424344, XattrBlkAddr: 0x51525354,
		UUID:        [16]uint8{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		VolumeName:  [16]uint8{16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1},
		ComprAlgs:   0x6162,
		DevtSlotOff: 9, DirBlkBits: 12, XattrPrefixCount: 7, XattrPrefixStart: 0x71727374,
		PackedNid: 0x8182838485868788, XattrFilterRes: 0x91,
		FeatureIncompat: FeatureIncompatChunkedFile, ExtraDevices: 2,
	}
	for i := range sb.Reserved {
		sb.Reserved[i] = uint8(i + 1)
	}

	var want bytes.Buffer
	if err := binary.Write(&want, binary.LittleEndian, &sb); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, SizeSuperBlock)
	sb.Marshal(got)
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("Marshal disagrees with binary.Write:\n got %x\nwant %x", got, want.Bytes())
	}

	var back SuperBlock
	back.Unmarshal(got)
	if back != sb {
		t.Errorf("Unmarshal(Marshal(sb)) = %+v, want %+v", back, sb)
	}
}

func TestDeviceSlotMarshalMatchesReflection(t *testing.T) {
	var ds DeviceSlot
	for i := range ds.Tag {
		ds.Tag[i] = uint8(i)
	}
	ds.Blocks = 0x01020304
	ds.MappedBlkAddr = 0x05060708
	for i := range ds.Reserved {
		ds.Reserved[i] = uint8(255 - i)
	}

	var want bytes.Buffer
	if err := binary.Write(&want, binary.LittleEndian, &ds); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, SizeDeviceSlot)
	ds.Marshal(got)
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("Marshal disagrees with binary.Write:\n got %x\nwant %x", got, want.Bytes())
	}

	var back DeviceSlot
	back.Unmarshal(got)
	if back != ds {
		t.Error("Unmarshal(Marshal(ds)) did not round-trip")
	}
}
