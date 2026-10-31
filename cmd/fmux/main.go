package main

import (
	"fmt"
	"os"

	"github.com/linxu/fmux/internal/api"
	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "fmux",
		Short: "Unified local inference gateway for macOS",
		Long: "fmux maps Ollama, llama.cpp, MLX, LM Studio, vLLM and SGLang behind one\n" +
			"OpenAI-compatible endpoint, so the fm CLI (or any other client) can drive\n" +
			"all of them through a single alias namespace.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       api.Version,
	}
	root.SetVersionTemplate("fmux {{.Version}}\n")

	root.AddCommand(newServeCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newBackendsCmd())
	root.AddCommand(newModelsCmd())
	root.AddCommand(newPsCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newVersionCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("fmux %s\n", api.Version)
			return nil
		},
	}
}
