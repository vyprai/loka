package lokametrics

import (
	"testing"
)

func TestSparklineNormal(t *testing.T) {
	values := []float64{0, 25, 50, 75, 100}
	result := RenderSparkline(values, len(values))

	if len([]rune(result)) != 5 {
		t.Fatalf("expected 5 characters, got %d: %q", len([]rune(result)), result)
	}

	runes := []rune(result)
	// Values go from 0 to 100, so first char should be the lowest block
	// and last char should be the highest block.
	if runes[0] != sparkBlocks[0] {
		t.Errorf("expected first char to be lowest block %c, got %c", sparkBlocks[0], runes[0])
	}
	if runes[4] != sparkBlocks[len(sparkBlocks)-1] {
		t.Errorf("expected last char to be highest block %c, got %c", sparkBlocks[len(sparkBlocks)-1], runes[4])
	}

	// Chars should be in ascending order.
	for i := 1; i < len(runes); i++ {
		if runes[i] < runes[i-1] {
			t.Errorf("expected ascending blocks, but position %d (%c) < position %d (%c)", i, runes[i], i-1, runes[i-1])
		}
	}
}

func TestSparklineIdentical(t *testing.T) {
	values := []float64{50, 50, 50, 50}
	result := RenderSparkline(values, len(values))

	runes := []rune(result)
	if len(runes) != 4 {
		t.Fatalf("expected 4 characters, got %d", len(runes))
	}

	// All values are the same so all chars should be the same.
	for i := 1; i < len(runes); i++ {
		if runes[i] != runes[0] {
			t.Errorf("expected all same char, but position %d (%c) != position 0 (%c)", i, runes[i], runes[0])
		}
	}
}

func TestSparklineSingleValue(t *testing.T) {
	values := []float64{42}
	result := RenderSparkline(values, 1)

	runes := []rune(result)
	if len(runes) != 1 {
		t.Fatalf("expected 1 character, got %d", len(runes))
	}
}

func TestSparklineEmpty(t *testing.T) {
	result := RenderSparkline([]float64{}, 10)
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}

	result = RenderSparkline(nil, 10)
	if result != "" {
		t.Errorf("expected empty string for nil, got %q", result)
	}
}

func TestSparklineWidth(t *testing.T) {
	// 10 values bucketed into 3 characters.
	values := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	result := RenderSparkline(values, 3)

	runes := []rune(result)
	if len(runes) != 3 {
		t.Fatalf("expected 3 characters, got %d: %q", len(runes), result)
	}

	// First bucket averages low values, last bucket averages high values,
	// so chars should be ascending.
	if runes[0] > runes[2] {
		t.Errorf("expected first bucket char <= last bucket char, got %c > %c", runes[0], runes[2])
	}
}
