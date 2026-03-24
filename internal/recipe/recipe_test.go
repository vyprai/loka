package recipe

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadBuiltin(t *testing.T) {
	builtins := []struct {
		name        string
		wantPort    bool
		wantImage   bool
		wantBuild   bool
		wantStart   bool
		wantMatch   bool
	}{
		{name: "nextjs", wantPort: true, wantImage: true, wantBuild: true, wantStart: true, wantMatch: true},
		{name: "vite", wantPort: true, wantImage: true, wantBuild: true, wantStart: true, wantMatch: true},
		{name: "bun", wantPort: true, wantImage: true, wantBuild: true, wantStart: true, wantMatch: true},
		{name: "nodejs", wantPort: true, wantImage: true, wantBuild: true, wantStart: true, wantMatch: true},
		{name: "python", wantPort: true, wantImage: true, wantStart: true, wantMatch: true},
		{name: "go", wantPort: true, wantImage: true, wantBuild: true, wantStart: true, wantMatch: true},
		{name: "static", wantPort: true, wantImage: true, wantStart: true, wantMatch: true},
	}

	for _, tt := range builtins {
		t.Run(tt.name, func(t *testing.T) {
			r, err := LoadBuiltin(tt.name)
			require.NoError(t, err)
			assert.Equal(t, tt.name, r.Name)
			assert.NotEmpty(t, r.Version)
			assert.NotEmpty(t, r.Description)
			assert.Equal(t, "builtin:"+tt.name, r.Source())
			if tt.wantPort {
				assert.Greater(t, r.Port, 0)
			}
			if tt.wantImage {
				assert.NotEmpty(t, r.Image)
			}
			if tt.wantBuild {
				assert.NotEmpty(t, r.Build)
			}
			if tt.wantStart {
				assert.NotEmpty(t, r.Start)
			}
			if tt.wantMatch {
				assert.NotEmpty(t, r.MatchScript())
			}
		})
	}
}

func TestLoadBuiltinNotFound(t *testing.T) {
	_, err := LoadBuiltin("nonexistent")
	assert.Error(t, err)
}

func TestListAll(t *testing.T) {
	recipes, err := ListAll()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(recipes), 7)

	names := make(map[string]bool)
	for _, r := range recipes {
		names[r.Name] = true
	}
	for _, expected := range []string{"nextjs", "vite", "bun", "nodejs", "python", "go", "static"} {
		assert.True(t, names[expected], "expected built-in recipe %q", expected)
	}
}

func TestTopologicalSort(t *testing.T) {
	recipes := []*Recipe{
		{Name: "static"},
		{Name: "nodejs", Before: []string{"static"}},
		{Name: "nextjs", Before: []string{"nodejs"}},
		{Name: "vite", Before: []string{"nodejs"}},
	}

	sorted := TopologicalSort(recipes)
	require.Len(t, sorted, 4)

	// Build index map.
	idx := make(map[string]int)
	for i, r := range sorted {
		idx[r.Name] = i
	}

	// nextjs and vite must come before nodejs.
	assert.Less(t, idx["nextjs"], idx["nodejs"], "nextjs should come before nodejs")
	assert.Less(t, idx["vite"], idx["nodejs"], "vite should come before nodejs")
	// nodejs must come before static.
	assert.Less(t, idx["nodejs"], idx["static"], "nodejs should come before static")
}

func TestTopologicalSortCycle(t *testing.T) {
	recipes := []*Recipe{
		{Name: "a", Before: []string{"b"}},
		{Name: "b", Before: []string{"c"}},
		{Name: "c", Before: []string{"a"}},
	}

	sorted := TopologicalSort(recipes)
	// All recipes should still be returned even with a cycle.
	assert.Len(t, sorted, 3)
	names := make(map[string]bool)
	for _, r := range sorted {
		names[r.Name] = true
	}
	assert.True(t, names["a"])
	assert.True(t, names["b"])
	assert.True(t, names["c"])
}

func TestTopologicalSortEmpty(t *testing.T) {
	sorted := TopologicalSort(nil)
	assert.Empty(t, sorted)
}

func TestMatchNextjs(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"dependencies":{"next":"14.0.0"}}`), 0644)
	recipe, err := Match(dir)
	require.NoError(t, err)
	require.NotNil(t, recipe)
	assert.Equal(t, "nextjs", recipe.Name)
}

func TestMatchNodejs(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"dependencies":{"express":"4.0.0"}}`), 0644)
	recipe, err := Match(dir)
	require.NoError(t, err)
	require.NotNil(t, recipe)
	assert.Equal(t, "nodejs", recipe.Name)
}

func TestMatchPython(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask==2.0.0\n"), 0644)
	recipe, err := Match(dir)
	require.NoError(t, err)
	require.NotNil(t, recipe)
	assert.Equal(t, "python", recipe.Name)
}

func TestMatchGo(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/myapp\n\ngo 1.21\n"), 0644)
	recipe, err := Match(dir)
	require.NoError(t, err)
	require.NotNil(t, recipe)
	assert.Equal(t, "go", recipe.Name)
}

func TestMatchStatic(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html></html>"), 0644)
	recipe, err := Match(dir)
	require.NoError(t, err)
	require.NotNil(t, recipe)
	assert.Equal(t, "static", recipe.Name)
}

func TestMatchVite(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"dependencies":{"vue":"3.0.0"}}`), 0644)
	os.WriteFile(filepath.Join(dir, "vite.config.js"), []byte("export default {}"), 0644)
	recipe, err := Match(dir)
	require.NoError(t, err)
	require.NotNil(t, recipe)
	assert.Equal(t, "vite", recipe.Name)
}

func TestMatchNoMatch(t *testing.T) {
	dir := t.TempDir()
	recipe, err := Match(dir)
	require.NoError(t, err)
	assert.Nil(t, recipe)
}

func TestRecipeContext(t *testing.T) {
	dir := t.TempDir()

	// Write a JSON file to test ReadJSON.
	jsonContent := `{"name":"testapp","version":"1.0.0"}`
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(jsonContent), 0644)
	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hello world"), 0644)

	r := &Recipe{Name: "test"}
	ctx := NewRecipeContext(dir, r)

	// FileExists
	assert.True(t, ctx.FileExists("config.json"))
	assert.True(t, ctx.FileExists("readme.txt"))
	assert.False(t, ctx.FileExists("nonexistent.txt"))

	// ReadFile
	content := ctx.ReadFile("readme.txt")
	assert.Equal(t, "hello world", content)
	assert.Empty(t, ctx.ReadFile("nonexistent.txt"))

	// ReadJSON
	parsed := ctx.ReadJSON("config.json")
	require.NotNil(t, parsed)
	assert.Equal(t, "testapp", parsed["name"])
	assert.Equal(t, "1.0.0", parsed["version"])
	assert.Nil(t, ctx.ReadJSON("nonexistent.json"))
	assert.Nil(t, ctx.ReadJSON("readme.txt")) // not valid JSON
}

func TestRecipeContextListFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.js"), []byte(""), 0644)
	os.WriteFile(filepath.Join(dir, "b.js"), []byte(""), 0644)
	os.WriteFile(filepath.Join(dir, "c.txt"), []byte(""), 0644)

	r := &Recipe{Name: "test"}
	ctx := NewRecipeContext(dir, r)

	jsFiles := ctx.ListFiles("*.js")
	assert.Len(t, jsFiles, 2)
}

func TestRecipeContextMutations(t *testing.T) {
	r := &Recipe{Name: "test", Port: 3000, Build: []string{"npm build"}, Start: "node server.js"}
	ctx := NewRecipeContext(t.TempDir(), r)

	ctx.SetPort(8080)
	assert.Equal(t, 8080, r.Port)

	ctx.SetEnv("NODE_ENV", "production")
	assert.Equal(t, "production", r.Env["NODE_ENV"])

	ctx.SetImage("node:20-alpine")
	assert.Equal(t, "node:20-alpine", r.Image)

	ctx.SetBuildCommand(0, "npm run build")
	assert.Equal(t, "npm run build", r.Build[0])

	ctx.SetStartCommand("node dist/server.js")
	assert.Equal(t, "node dist/server.js", r.Start)
}
