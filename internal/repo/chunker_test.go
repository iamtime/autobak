package repo

import (
	"bytes"
	"crypto/sha256"
	"io"
	"math/rand"
	"testing"
)

func chunkAll(t *testing.T, data []byte, p ChunkerParams) ([][32]byte, []int) {
	t.Helper()
	c := NewChunker(bytes.NewReader(data), p)
	var ids [][32]byte
	var sizes []int
	total := 0
	for {
		ch, err := c.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		ids = append(ids, sha256.Sum256(ch))
		sizes = append(sizes, len(ch))
		total += len(ch)
	}
	if total != len(data) {
		t.Fatalf("потеряны данные: собрано %d из %d байт", total, len(data))
	}
	return ids, sizes
}

// Данные, похожие на реальные: повторяющиеся блоки текста вперемешку со случайным.
func realisticData(seed int64, n int) []byte {
	r := rand.New(rand.NewSource(seed))
	words := [][]byte{
		[]byte("<?php namespace App; use IlluminateSupport; "),
		[]byte("INSERT INTO `orders` VALUES ("),
		[]byte("server { listen 443 ssl http2; root /home/admin/web; } "),
		[]byte("0123456789abcdef"),
	}
	var b bytes.Buffer
	b.Grow(n)
	for b.Len() < n {
		if r.Intn(8) == 0 {
			junk := make([]byte, r.Intn(4096))
			r.Read(junk)
			b.Write(junk)
		} else {
			b.Write(words[r.Intn(len(words))])
		}
	}
	return b.Bytes()[:n]
}

func TestChunkerBoundaries(t *testing.T) {
	p := DefaultChunkerParams(0xC0FFEE)
	data := realisticData(1, 64*MiB)
	_, sizes := chunkAll(t, data, p)

	for i, s := range sizes {
		last := i == len(sizes)-1
		if s > p.Max {
			t.Fatalf("чанк %d больше Max: %d", i, s)
		}
		if s < p.Min && !last {
			t.Fatalf("чанк %d меньше Min: %d", i, s)
		}
	}
	avg := len(data) / len(sizes)
	if avg < p.Avg/2 || avg > p.Avg*2 {
		t.Fatalf("средний чанк %d далёк от целевого %d", avg, p.Avg)
	}
	t.Logf("чанков %d, средний %d КБ", len(sizes), avg/KiB)
}

// Главное свойство CDC: правка в середине не должна сдвигать границы дальше
// по потоку. Без этого инкрементальный бэкап каждый раз перезаливал бы всё.
func TestChunkerShiftResistance(t *testing.T) {
	p := DefaultChunkerParams(0xC0FFEE)
	data := realisticData(2, 64*MiB)
	base, _ := chunkAll(t, data, p)

	var edited []byte
	edited = append(edited, data[:20*MiB]...)
	edited = append(edited, []byte("-- сюда вписали новую строку в дамп --")...)
	edited = append(edited, data[20*MiB:]...)
	after, _ := chunkAll(t, edited, p)

	have := make(map[[32]byte]bool, len(base))
	for _, id := range base {
		have[id] = true
	}
	reused := 0
	for _, id := range after {
		if have[id] {
			reused++
		}
	}
	ratio := float64(reused) / float64(len(after))
	if ratio < 0.95 {
		t.Fatalf("после вставки переиспользовано лишь %.1f%% чанков", ratio*100)
	}
	t.Logf("переиспользовано %.2f%% чанков (%d из %d)", ratio*100, reused, len(after))
}

// Разный seed обязан давать разные границы, иначе он не защищает от
// опознания содержимого по размерам чанков.
func TestChunkerSeedChangesBoundaries(t *testing.T) {
	data := realisticData(3, 16*MiB)
	a, _ := chunkAll(t, data, DefaultChunkerParams(1))
	b, _ := chunkAll(t, data, DefaultChunkerParams(2))
	same := 0
	for _, id := range a {
		for _, id2 := range b {
			if id == id2 {
				same++
				break
			}
		}
	}
	if same > len(a)/10 {
		t.Fatalf("разные seed дали %d одинаковых чанков из %d", same, len(a))
	}
}
