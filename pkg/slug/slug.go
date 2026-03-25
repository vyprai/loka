package slug

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

var adjectives = []string{
	"calm", "bold", "swift", "warm", "cool", "keen", "fair",
	"brave", "glad", "wise", "kind", "pure", "safe", "fast",
	"neat", "rich", "soft", "deep", "free", "true",
}

var nouns = []string{
	"river", "cloud", "storm", "light", "stone", "flame", "frost",
	"ocean", "creek", "brook", "ridge", "grove", "field", "bloom",
	"spark", "shade", "drift", "coral", "cedar", "maple",
}

// Generate returns a slug like "brave-falcon-a3f2".
func Generate() string {
	b := make([]byte, 4)
	rand.Read(b)
	hash := hex.EncodeToString(b)[:4]
	adj := adjectives[int(b[0])%len(adjectives)]
	noun := nouns[int(b[1])%len(nouns)]
	return fmt.Sprintf("%s-%s-%s", adj, noun, hash)
}
