package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func newPluginShowCommand(opts *commandOptions) *cobra.Command {
	var details bool
	cmd := &cobra.Command{
		Use: "show NAME", Short: "Show retained installation facts and current access", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			status, err := pluginStatusForDisplay(service.Store, args[0])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), strings.Join(pluginShowLines(service.Store, status, details), "\n"))
			return err
		},
	}
	cmd.Flags().BoolVar(&details, "details", false, "show retained hashes, exact argv and installer evidence; source and purpose are not retained")
	return cmd
}

func pluginShowLines(store bus.Store, status pluginfeed.PluginFeedStatus, details bool) []string {
	title := fmt.Sprintf("Plugin %q (manual installation; identity/version unknown)", status.Name)
	if status.Install != nil {
		title = fmt.Sprintf("Plugin %q (%s %s, author's claim)", status.Name, plainText(status.Install.ID), plainText(status.Install.Version))
	}
	state := "Disabled"
	if status.Enabled {
		state = "Enabled"
	}
	lines := []string{
		title,
		state + "; runtime not checked. Start it with: zalki plugins run " + status.Name,
		"Standard: " + pluginStandardText(status.Access),
		"Scope: " + pluginScopeText(status.Scope) + "; " + pluginEligibleSnapshot(store, status),
		"SEND: " + pluginExactTargets(status.Access.Send),
		"READ: " + pluginExactTargets(status.Access.Read),
		"Installed by: " + pluginInstallerText(status.Install),
	}
	if status.Install != nil {
		lines = append(lines, "Installed at: "+plainText(status.Install.InstalledAt), "Source recorded at install: "+plainText(status.Install.Source))
	}
	lines = append(lines,
		"Purpose: not retained after install.",
		"Staged source copy: not retained after install.",
		"It runs as you, with no sandbox. It can do anything your user can do.")
	if details {
		return append(lines, pluginShowDetails(status)...)
	}
	return append(lines, "Details: zalki plugins show "+status.Name+" --details")
}

func pluginExactTargets(ids []string) string {
	if len(ids) == 0 {
		return "nobody"
	}
	return strings.Join(ids, ", ") // validated canonical IDs; no name re-resolution
}

// No source folder, retained stage, tool or plugin is opened/executed here.
func pluginShowDetails(status pluginfeed.PluginFeedStatus) []string {
	var lines []string
	if receipt := status.Install; receipt != nil {
		lines = append(lines,
			"API (author's claim): "+plainText(receipt.API),
			"Source tree SHA256: "+receipt.TreeSHA256,
			"Manifest SHA256: "+receipt.ManifestSHA256,
			"Build argv: "+quotedList(receipt.Build),
			"Artifact: "+plainText(receipt.Artifact),
			"Artifact SHA256: "+receipt.ArtifactSHA256,
			"Installer evidence: "+plainText(receipt.InstalledBy),
			"Declared access: "+string(receipt.Access))
		if receipt.Tool != "" {
			lines = append(lines, "Recorded tool: "+plainText(receipt.Tool), "Recorded tool SHA256: "+receipt.ToolSHA256,
				"The recorded tool hash is a change check; the compiler was not pinned.")
		}
	}
	if launch := status.Launch; launch != nil {
		lines = append(lines, "Pinned executable: "+plainText(launch.Executable), "Run argv: "+quotedList(launch.Args), "Launch pin SHA256: "+launch.PinSHA256)
	}
	return append(lines, "Publisher proof: none (local folder)", installWarning)
}
