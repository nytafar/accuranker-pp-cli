// Hand-authored: filters command tree.
//
// Surfaces AccuRanker's 100+ dynamic-filter dimensions as a discoverable
// command subtree. An agent (or a forgetful human) can `filters describe
// search_intent` and learn it's an array taking any/all/none/empty before
// composing a --filter expression.
package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"accuranker-pp-cli/internal/schema"
)

func newFiltersCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "filters",
		Short: "Discover AccuRanker filter dimensions (100+ named, with legal comparators)",
		Annotations: map[string]string{
			"mcp:read-only": "true",
		},
	}
	cmd.AddCommand(newFiltersListCmd(flags))
	cmd.AddCommand(newFiltersDescribeCmd(flags))
	return cmd
}

func newFiltersListCmd(flags *rootFlags) *cobra.Command {
	var (
		includeLLM bool
		onlyLLM    bool
		classMatch string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all available filter dimensions",
		Example: strings.Trim(`
  # All non-LLM filter dimensions
  accuranker-pp-cli filters list

  # Include the LLM-tier filters (visible whether or not your plan can use them)
  accuranker-pp-cli filters list --include-llm

  # Only show array-class filters (e.g. tags, page_serp_features)
  accuranker-pp-cli filters list --class array

  # JSON for agents
  accuranker-pp-cli filters list --json
`, "\n"),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			model, err := schema.LoadDefault()
			if err != nil {
				return err
			}
			dims := make([]schema.FilterDimension, 0, len(model.FilterDimensions))
			for _, fd := range model.FilterDimensions {
				if onlyLLM && !fd.LLM {
					continue
				}
				if !onlyLLM && !includeLLM && fd.LLM {
					continue
				}
				if classMatch != "" && fd.Class != classMatch {
					continue
				}
				dims = append(dims, fd)
			}
			sort.Slice(dims, func(i, j int) bool { return dims[i].Name < dims[j].Name })

			if flags.asJSON {
				return writeJSONFilters(cmd, dims, model)
			}
			return writeTableFilters(cmd, dims, model)
		},
	}
	cmd.Flags().BoolVar(&includeLLM, "include-llm", false, "Include LLM-tier filter dimensions (paywalled for some plans)")
	cmd.Flags().BoolVar(&onlyLLM, "only-llm", false, "Show ONLY LLM-tier filter dimensions")
	cmd.Flags().StringVar(&classMatch, "class", "", "Filter by class: numeric, string, array, boolean, date, folder")
	return cmd
}

func newFiltersDescribeCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "describe <name>",
		Short: "Show a filter dimension's class and legal comparators",
		Example: strings.Trim(`
  accuranker-pp-cli filters describe rank
  accuranker-pp-cli filters describe search_intent --json
  accuranker-pp-cli filters describe llm_brand --json
`, "\n"),
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			model, err := schema.LoadDefault()
			if err != nil {
				return err
			}
			fd := model.FilterByName(args[0])
			if fd == nil {
				return fmt.Errorf("unknown filter dimension %q (use `accuranker-pp-cli filters list` to discover)", args[0])
			}
			comparators := model.ComparatorsFor(fd.Name)
			payload := map[string]any{
				"name":        fd.Name,
				"class":       fd.Class,
				"llm":         fd.LLM,
				"comparators": comparators,
			}
			if len(fd.ValueSet) > 0 {
				payload["value_set"] = fd.ValueSet
			}
			if flags.asJSON {
				return writeJSON(cmd, payload, flags)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Filter: %s\n", fd.Name)
			fmt.Fprintf(cmd.OutOrStdout(), "  Class:       %s\n", fd.Class)
			fmt.Fprintf(cmd.OutOrStdout(), "  Comparators: %s\n", strings.Join(comparators, ", "))
			if len(fd.ValueSet) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "  Value set:   %s\n", strings.Join(fd.ValueSet, ", "))
			}
			if fd.LLM {
				fmt.Fprintln(cmd.OutOrStdout(), "  Plan tier:   AccuLLM (LLM API plan required)")
			}
			return nil
		},
	}
	return cmd
}

func writeJSONFilters(cmd *cobra.Command, dims []schema.FilterDimension, model *schema.Model) error {
	out := make([]map[string]any, 0, len(dims))
	for _, fd := range dims {
		entry := map[string]any{
			"name":        fd.Name,
			"class":       fd.Class,
			"comparators": model.ComparatorsFor(fd.Name),
			"llm":         fd.LLM,
		}
		if len(fd.ValueSet) > 0 {
			entry["value_set"] = fd.ValueSet
		}
		out = append(out, entry)
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func writeTableFilters(cmd *cobra.Command, dims []schema.FilterDimension, model *schema.Model) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tCLASS\tLLM\tCOMPARATORS")
	for _, fd := range dims {
		llmTag := ""
		if fd.LLM {
			llmTag = "llm"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", fd.Name, fd.Class, llmTag, strings.Join(model.ComparatorsFor(fd.Name), ","))
	}
	return w.Flush()
}
