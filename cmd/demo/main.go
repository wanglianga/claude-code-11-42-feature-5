// Command demo runs the end-to-end scenario, prints a report and exits
// non-zero if any check fails. It is the one-command verification entry point.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"cargo.example/secstate/internal/demo"
	"cargo.example/secstate/internal/service"
	"cargo.example/secstate/internal/store"
)

func main() {
	dsn := flag.String("dsn", envOr("DATABASE_URL",
		"postgres://ccdev:ccdev@127.0.0.1:5432/ccdev?sslmode=disable"), "PostgreSQL DSN")
	out := flag.String("out", "", "optional path to write the JSON report")
	markdown := flag.Bool("markdown", false, "print a markdown-friendly table instead of JSON")
	reset := flag.Bool("reset", true, "reset the database schema before running (the demo needs a clean, deterministic ledger)")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	st, err := store.Open(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		os.Exit(2)
	}
	defer st.Close()

	svc := service.New(st, service.DefaultConfig())
	runner := demo.NewRunner(svc)
	runner.ResetBeforeRun = *reset
	rep, err := runner.Run(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run demo:", err)
		os.Exit(2)
	}

	if *out != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write report:", err)
			os.Exit(2)
		}
	}
	if *markdown {
		printMarkdown(rep)
	} else {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
	}
	failed := 0
	for _, c := range rep.Checks {
		if !c.OK {
			failed++
		}
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "\n%d check(s) failed\n", failed)
		os.Exit(1)
	}
}

func printMarkdown(rep *demo.Report) {
	fmt.Print("# 演示报告\n\n")
	for _, sec := range rep.Sections {
		fmt.Printf("## %s\n\n", sec.Title)
		fmt.Println("| 步骤 | 结果 | 说明 |")
		fmt.Println("| --- | --- | --- |")
		for _, s := range sec.Steps {
			mark := "✅"
			if !s.OK {
				mark = "❌"
			}
			fmt.Printf("| %s | %s | %s |\n", esc(s.Name), mark, esc(s.Detail))
		}
		fmt.Println()
	}
	fmt.Print("## 断言检查\n\n")
	fmt.Println("| 检查 | 结果 | 依据 |")
	fmt.Println("| --- | --- | --- |")
	for _, c := range rep.Checks {
		mark := "✅"
		if !c.OK {
			mark = "❌"
		}
		fmt.Printf("| %s | %s | %s |\n", esc(c.Name), mark, esc(c.Detail))
	}
	fmt.Printf("\n**%s**\n", rep.Summary)
}

func esc(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '|' {
			out = append(out, '\\', '|')
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
