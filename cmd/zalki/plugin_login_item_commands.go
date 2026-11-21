package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/install"
)

// installedPlugins counts owner records. A record that cannot be read counts
// as installed and enabled, so no line or question rests on a guess.
func installedPlugins(store bus.Store) (total, enabled int, err error) {
	dir := filepath.Join(store.Dir(), "plugins")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, pluginsFolderProblem(dir, err)
	}
	for _, entry := range entries {
		match := pluginRecordName.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		total++
		status, err := pluginStatusForDisplay(store, match[1])
		if err != nil || !entry.Type().IsRegular() || status.Enabled {
			enabled++
		}
	}
	return total, enabled, nil
}

// rootSetup is the login-item part of `zalki install --apply`: the same rows
// as a plugin install without the build, and at most one question.
func (t *loginItemTarget) rootSetup(cmd *cobra.Command, store bus.Store) error {
	if _, enabled, err := installedPlugins(store); err != nil || enabled == 0 {
		return err
	}
	ctx := cmd.Context()
	plan := t.inspect(ctx)
	choice, restore := installTerminal(cmd.InOrStdin())
	defer func() { restore() }()
	if choice == installNoTerminal {
		if plan.row == rowFirst {
			return sayLine(cmd, loginItemRootNotSetUp)
		}
		return nil
	}
	if plan.seen.pending {
		// An unfinished change the owner already agreed to: 7c resumes it
		// behind its fences, or refuses a request for another launcher.
		return t.rootResult(ctx, cmd, t.request("install", true), loginItemRootStarted)
	}
	lines, question, done := rootQuestion(plan)
	if lines != "" {
		if err := sayLine(cmd, lines); err != nil {
			return err
		}
	}
	if question == "" {
		return nil
	}
	yes, err := askOnce(ctx, cmd, choice == installRawKeys, question)
	if err != nil || !yes {
		return err
	}
	action := "install"
	if plan.row == rowRelaunch {
		action = "relaunch"
	}
	return t.rootResult(ctx, cmd, t.change(action, plan), done)
}

func (t *loginItemTarget) rootResult(ctx context.Context, cmd *cobra.Command, request install.Request, done string) error {
	if _, err := loginItems(ctx, request); err != nil {
		return fmt.Errorf(loginItemRootFailed, trimSentence(err))
	}
	return sayLine(cmd, done)
}

// rootQuestion is the root decision table: the lines shown, the one
// question ("" = no change) and the result line after a made request.
func rootQuestion(plan loginItemPlan) (lines, question, done string) {
	switch {
	case plan.row == rowFirst:
		return loginItemConsentLine + "\n" + loginItemFirstLine, loginItemRootAsk, loginItemRootStarted
	case plan.row == rowOwned && plan.seen.service == "not_loaded":
		// Our item on this launcher, not running: start it after the
		// consent line and the one question (the plugin table's owned row).
		return loginItemConsentLine, loginItemRootAsk, loginItemRootStarted
	case plan.row == rowRelaunch:
		return "", fmt.Sprintf(loginItemRelaunchAsk, plainText(plan.launcher), plainText(plan.binary)), fmt.Sprintf(loginItemRelaunchedLine, plainText(plan.binary))
	case plan.row == rowOwned:
		return "", "", ""
	}
	return plan.disclosure()[0], "", ""
}

// askOnce asks one [y/N] question (three tries for an invalid key).
func askOnce(ctx context.Context, cmd *cobra.Command, raw bool, question string) (bool, error) {
	input := newContextReader(ctx, cmd.InOrStdin())
	input.maxRead = 1
	keys := bufio.NewReader(input)
	for range 3 {
		if _, err := fmt.Fprint(cmd.OutOrStdout(), question+" "); err != nil {
			return false, err
		}
		answer, err := readPluginReviewAnswer(keys, raw)
		if err != nil {
			return false, err // an interrupted question changes nothing
		}
		if err := sayLine(cmd, ""); err != nil {
			return false, err
		}
		switch answer {
		case pluginReviewAccept:
			return true, nil
		case pluginReviewCancel:
			return false, nil
		case pluginReviewInvalid:
		}
	}
	return false, nil
}

// uninstallNote removes the owned item for `zalki uninstall --apply` (no
// question) or names it in the preview. Foreign or unknown items are left
// alone and get no note.
func (t *loginItemTarget) uninstallNote(ctx context.Context, apply bool) (string, error) {
	plan := t.inspect(ctx)
	ours := plan.seen.pending || plan.seen.launcher != ""
	switch {
	case !ours:
		return "", nil
	case !apply:
		return loginItemWouldRemoveNote, nil
	}
	if _, err := loginItems(ctx, t.request("uninstall", true)); err != nil {
		if t.inspect(ctx).seen.service == "unknown" {
			return t.manualRemoval(), err // the way out by hand
		}
		return "", err
	}
	return loginItemRemovedNote, nil
}

// manualRemoval is the by-hand removal of the owned item when its job
// cannot be checked: confirm it is ours, stop it, and delete the files only
// once launchd no longer knows the job.
func (t *loginItemTarget) manualRemoval() string {
	service := "gui/" + strconv.Itoa(os.Getuid()) + "/" + backgroundLabel
	plugins := filepath.Join(t.state, "plugins")
	return fmt.Sprintf(loginItemManualRemoval, service, t.binary, shellQuote(t.state), service, service,
		shellQuote(filepath.Join(t.home, "Library", "LaunchAgents", backgroundLabel+".plist")),
		shellQuote(filepath.Join(plugins, "login-item.installation.json")),
		shellQuote(filepath.Join(plugins, "login-item.installation.pending.json")))
}

// kickstart asks launchd to start the owned job again without killing it,
// only with fresh evidence that it is ours, loaded, matching and not
// disabled. It reports whether the request was made.
func (t *loginItemTarget) kickstart(ctx context.Context) (bool, error) {
	plan := t.inspect(ctx)
	if plan.row != rowOwned || plan.seen.service != "loaded" || plan.seen.loaded != "matches" {
		return false, nil
	}
	if _, err := loginItems(ctx, t.request("upgrade", true)); err != nil {
		return false, err
	}
	return true, nil
}

// idleLine is the idle line after the last plugin was removed while the
// owned item stays.
func (t *loginItemTarget) idleLine(ctx context.Context, store bus.Store) string {
	if total, _, err := installedPlugins(store); err != nil || total != 0 {
		return ""
	}
	if t.inspect(ctx).seen.launcher == "" {
		return ""
	}
	return loginItemIdleLine
}

func sayLine(cmd *cobra.Command, line string) error {
	_, err := fmt.Fprintln(cmd.OutOrStdout(), line)
	return err
}

func trimSentence(err error) string {
	text := err.Error()
	if n := len(text); n > 0 && text[n-1] == '.' {
		return text[:n-1]
	}
	return text
}
