package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"yoyaku/internal/scenario"
)

func scenarios(root string, args []string, out io.Writer) error {
	c, e := scenario.Load(root)
	if e != nil {
		return e
	}
	if len(args) == 0 || len(args) == 1 && args[0] == "list" {
		for _, s := range c.Scenarios {
			fmt.Fprintf(out, "%-30s [%s] %s\n", s.ID, s.Category, s.Title)
		}
		return nil
	}
	if len(args) == 1 && args[0] == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(c)
	}
	if len(args) == 2 && args[0] == "show" {
		s, ok := c.Find(args[1])
		if !ok {
			return fmt.Errorf("Unknown reservation case: %s", args[1])
		}
		fmt.Fprintf(out, "%s · %s\n%s\n\n직접 확인할 항목:\n", s.ID, s.Title, s.Description)
		for _, check := range s.Checks {
			fmt.Fprintln(out, "  [ ]", check)
		}
		for _, turn := range s.Turns {
			if turn.Speaker == "A" {
				fmt.Fprintf(out, "\nA/%s [JA] %s\n     [기대 의미] %s\n", turn.ID, turn.Text, turn.KO)
			} else {
				fmt.Fprintf(out, "\nB/%s [KO 예시] %s\n     [JA 참고] %s\n", turn.ID, turn.KO, turn.Japanese)
			}
		}
		return nil
	}
	return fmt.Errorf("Use scenarios list | scenarios show CASE | scenarios json")
}
