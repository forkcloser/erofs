package erofs

type block struct {
	buf    []byte
	offset int32
	end    int32
}

// bytes is the loaded window of the block. Every block comes from the image's
// pool with its buffer allocated, so the window is never nil.
func (b *block) bytes() []byte {
	return b.buf[b.offset:b.end]
}

func calculateBlocks(blockBits uint8, size int64) int {
	blockNum := size >> blockBits
	if size > blockNum<<blockBits {
		blockNum++
	}

	return int(blockNum)
}
