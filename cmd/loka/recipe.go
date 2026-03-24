package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/vyprai/loka/internal/recipe"
)

func newRecipeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recipe",
		Short: "Manage deployment recipes",
		Long: `List, add, or remove recipes for app deployment.

Recipes define how to build and deploy different project types (Next.js, Go, Python, etc.).
Built-in recipes are included with LOKA. Custom recipes can be added from git repositories.

Examples:
  loka recipe list
  loka recipe add https://github.com/user/my-recipe.git
  loka recipe remove my-recipe`,
	}
	cmd.AddCommand(
		newRecipeListCmd(),
		newRecipeAddCmd(),
		newRecipeRemoveCmd(),
	)
	return cmd
}

func newRecipeListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List available recipes",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			recipes, err := recipe.ListAll()
			if err != nil {
				return fmt.Errorf("list recipes: %w", err)
			}

			if outputFmt == "json" {
				return printJSON(recipes)
			}

			if len(recipes) == 0 {
				fmt.Println("No recipes found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tVERSION\tSOURCE\tDESCRIPTION")
			for _, r := range recipes {
				source := r.Source()
				if strings.HasPrefix(source, "builtin:") {
					source = "built-in"
				} else {
					source = "installed"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
					r.Name, r.Version, source, r.Description)
			}
			w.Flush()
			return nil
		},
	}
}

func newRecipeAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <git-url>",
		Short: "Install a recipe from a git repository",
		Long: `Clone a recipe repository into ~/.loka/recipes/.

The repository must contain a recipe.yaml file at its root.

Examples:
  loka recipe add https://github.com/user/my-recipe.git
  loka recipe add git@github.com:user/my-recipe.git`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			gitURL := args[0]

			// Derive recipe name from URL.
			name := filepath.Base(gitURL)
			name = strings.TrimSuffix(name, ".git")

			targetDir := filepath.Join(recipe.UserRecipesDir(), name)

			// Check if already exists.
			if _, err := os.Stat(targetDir); err == nil {
				return fmt.Errorf("recipe %q already exists at %s. Remove it first: loka recipe remove %s", name, targetDir, name)
			}

			// Ensure parent directory exists.
			if err := os.MkdirAll(recipe.UserRecipesDir(), 0o755); err != nil {
				return fmt.Errorf("create recipes dir: %w", err)
			}

			// Clone.
			fmt.Printf("Cloning %s...\n", gitURL)
			gitCmd := exec.Command("git", "clone", "--depth", "1", gitURL, targetDir)
			gitCmd.Stdout = os.Stdout
			gitCmd.Stderr = os.Stderr
			if err := gitCmd.Run(); err != nil {
				return fmt.Errorf("git clone failed: %w", err)
			}

			// Verify recipe.yaml exists.
			recipeYAML := filepath.Join(targetDir, "recipe.yaml")
			if _, err := os.Stat(recipeYAML); err != nil {
				// Clean up.
				os.RemoveAll(targetDir)
				return fmt.Errorf("repository does not contain a recipe.yaml")
			}

			// Load and display info.
			r, err := recipe.LoadFromDir(targetDir)
			if err != nil {
				os.RemoveAll(targetDir)
				return fmt.Errorf("invalid recipe: %w", err)
			}

			fmt.Printf("Installed recipe: %s\n", r.String())
			fmt.Printf("  Location: %s\n", targetDir)
			return nil
		},
	}
}

func newRecipeRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <name>",
		Short:   "Remove an installed recipe",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			targetDir := filepath.Join(recipe.UserRecipesDir(), name)

			if _, err := os.Stat(targetDir); err != nil {
				return fmt.Errorf("recipe %q not found at %s", name, targetDir)
			}

			if err := os.RemoveAll(targetDir); err != nil {
				return fmt.Errorf("remove recipe: %w", err)
			}

			fmt.Printf("Removed recipe %q\n", name)
			return nil
		},
	}
}
