package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func renderCommandHelp(command *cobra.Command, _ []string) {
	localizeDefaultFlags(command)
	writer := command.OutOrStdout()
	description := strings.TrimSpace(command.Long)
	if description == "" {
		description = command.Short
	}
	fmt.Fprintln(writer, description)
	fmt.Fprintf(writer, "\n用法：\n  %s\n", command.UseLine())
	if command.Parent() == nil {
		fmt.Fprintln(writer, "\n全部命令：")
		for _, available := range availableHelpCommands(command) {
			fmt.Fprintf(writer, "  %-36s %s\n", available.CommandPath(), available.Short)
		}
	} else if command.HasAvailableSubCommands() {
		fmt.Fprintln(writer, "\n可用子命令：")
		for _, child := range command.Commands() {
			if child.IsAvailableCommand() && child.Name() != "help" {
				fmt.Fprintf(writer, "  %-18s %s\n", child.Name(), child.Short)
			}
		}
	}
	writeCommandFlags(writer, command)
	if strings.TrimSpace(command.Example) != "" {
		fmt.Fprintf(writer, "\n示例：\n%s\n", command.Example)
	}
}

func availableHelpCommands(command *cobra.Command) []*cobra.Command {
	var result []*cobra.Command
	for _, child := range command.Commands() {
		if child.Name() == "help" || !child.IsAvailableCommand() {
			continue
		}
		result = append(result, child)
		if child.HasAvailableSubCommands() {
			result = append(result, availableHelpCommands(child)...)
		}
	}
	return result
}

func localizeDefaultFlags(command *cobra.Command) {
	command.InitDefaultHelpFlag()
	if helpFlag := command.Flags().Lookup("help"); helpFlag != nil {
		helpFlag.Usage = "显示帮助"
	}
	if command.Parent() == nil {
		command.InitDefaultVersionFlag()
		if versionFlag := command.Flags().Lookup("version"); versionFlag != nil {
			versionFlag.Usage = "显示版本"
		}
	}
}

func writeCommandFlags(writer io.Writer, command *cobra.Command) {
	writeFlagSet(writer, "参数", command.NonInheritedFlags())
	writeFlagSet(writer, "全局参数", command.InheritedFlags())
}

func writeFlagSet(writer io.Writer, title string, flags *pflag.FlagSet) {
	if flags == nil || !flags.HasAvailableFlags() {
		return
	}
	fmt.Fprintf(writer, "\n%s：\n%s", title, flags.FlagUsages())
}

func writeNearestCommandHelp(root *cobra.Command, args []string, writer io.Writer) {
	command := nearestCommand(root, args)
	original := command.OutOrStdout()
	command.SetOut(writer)
	_ = command.Help()
	command.SetOut(original)
}

func nearestCommand(root *cobra.Command, args []string) *cobra.Command {
	current := root
	index := 0
	if len(args) > 0 && args[0] == "help" {
		index++
	}
	for index < len(args) {
		argument := args[index]
		switch {
		case argument == "--config":
			index += 2
			continue
		case strings.HasPrefix(argument, "--config="), argument == "--json", argument == "--version":
			index++
			continue
		case strings.HasPrefix(argument, "-"):
			index++
			continue
		}
		var matched *cobra.Command
		for _, child := range current.Commands() {
			if child.Name() == argument {
				matched = child
				break
			}
		}
		if matched == nil {
			break
		}
		current = matched
		index++
	}
	return current
}
