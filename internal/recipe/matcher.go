package recipe

import (
	"fmt"
	"log/slog"
)

// Match detects the project type by running all recipe match.js hooks
// in topological order (based on `before` declarations). Returns the
// first matching recipe, or nil if no recipe matches.
func Match(projectDir string) (*Recipe, error) {
	recipes, err := ListAll()
	if err != nil {
		return nil, fmt.Errorf("list recipes: %w", err)
	}

	sorted := TopologicalSort(recipes)

	for _, r := range sorted {
		if r.matchScript == "" {
			slog.Debug("recipe has no match script, skipping", "name", r.Name)
			continue
		}

		ctx := NewRecipeContext(projectDir, r)
		val, err := ctx.RunScript(r.matchScript)
		if err != nil {
			slog.Warn("recipe match script failed", "name", r.Name, "error", err)
			continue
		}

		if val != nil && val.ToBoolean() {
			slog.Info("recipe matched", "name", r.Name, "project", projectDir)
			return r, nil
		}
	}

	return nil, nil
}

// TopologicalSort orders recipes so that recipes declaring `before: [X]`
// are checked before recipe X. This uses Kahn's algorithm.
func TopologicalSort(recipes []*Recipe) []*Recipe {
	if len(recipes) == 0 {
		return recipes
	}

	// Build a map of recipe name -> recipe.
	byName := make(map[string]*Recipe, len(recipes))
	for _, r := range recipes {
		byName[r.Name] = r
	}

	// Build adjacency: if recipe A has before: [B], then A -> B (A comes before B).
	// In-degree counts how many recipes must come before a given recipe.
	inDegree := make(map[string]int, len(recipes))
	// edges[A] = list of recipes that A must come before.
	edges := make(map[string][]string, len(recipes))

	for _, r := range recipes {
		if _, ok := inDegree[r.Name]; !ok {
			inDegree[r.Name] = 0
		}
		for _, target := range r.Before {
			if _, exists := byName[target]; !exists {
				continue // Skip references to unknown recipes.
			}
			edges[r.Name] = append(edges[r.Name], target)
			inDegree[target]++
		}
	}

	// Start with recipes that have no incoming edges (nothing must come before them).
	var queue []string
	for _, r := range recipes {
		if inDegree[r.Name] == 0 {
			queue = append(queue, r.Name)
		}
	}

	var sorted []*Recipe
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]

		if r, ok := byName[name]; ok {
			sorted = append(sorted, r)
		}

		for _, target := range edges[name] {
			inDegree[target]--
			if inDegree[target] == 0 {
				queue = append(queue, target)
			}
		}
	}

	// If there are recipes not in sorted (cycle or disconnected), append them.
	seen := make(map[string]bool, len(sorted))
	for _, r := range sorted {
		seen[r.Name] = true
	}
	for _, r := range recipes {
		if !seen[r.Name] {
			sorted = append(sorted, r)
		}
	}

	return sorted
}
