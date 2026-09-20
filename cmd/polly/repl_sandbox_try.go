package main

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// sandboxTryOutputLines bounds the output the line frontend prints.
const sandboxTryOutputLines = 40

// sandboxTryCommand runs /sandbox try. With a command it runs a trial of it
// and a review of what the sandbox denied it, in the managed TUI's dialog or
// as text; without one, it offers the session's recent failed bash
// commands.
func sandboxTryCommand(ctx *replCommandContext) []string {
	if _, why := sandboxProfileFor(ctx); why != "" {
		return []string{why}
	}
	command := commandArgument(ctx.line, 2)
	if command == "" {
		commands, err := recentFailedCommands(ctx.operationContext(), ctx.state)
		if err != nil {
			return []string{"sandbox try: " + err.Error()}
		}
		if len(commands) == 0 {
			return []string{"usage: /sandbox try <command>; this session has no failed bash command to offer"}
		}
		if ctx.pickSandboxTry != nil {
			ctx.pickSandboxTry(commands)
			return nil
		}
		if command = pickSandboxTryLine(ctx, commands); command == "" {
			return nil
		}
	}
	try, err := newSandboxTry(ctx.state, command)
	if err != nil {
		return []string{"sandbox try: " + err.Error()}
	}
	if ctx.sandboxTry != nil {
		ctx.sandboxTry(try)
		return nil
	}
	return reviewSandboxTryLines(ctx, try)
}

// commandArgument is what follows the first n fields of a command line, as
// typed.
func commandArgument(line string, n int) string {
	rest := strings.TrimSpace(line)
	for range n {
		i := strings.IndexFunc(rest, unicode.IsSpace)
		if i < 0 {
			return ""
		}
		rest = strings.TrimSpace(rest[i:])
	}
	return rest
}

// pickSandboxTryLine lists the failed commands and, where it can ask, reads
// which to try; "" means none.
func pickSandboxTryLine(ctx *replCommandContext, commands []string) string {
	lines := []string{"recent failed bash commands:"}
	for i, command := range commands {
		lines = append(lines, fmt.Sprintf("  %d. %s", i+1, commandLine(command)))
	}
	if ctx.readInput == nil {
		_ = ctx.replyLines(append(lines, "try one with /sandbox try <command>"))
		return ""
	}
	if err := ctx.replyLines(lines); err != nil {
		return ""
	}
	answer, err := ctx.readInput("try which (a number; Enter for none)? ")
	if err != nil {
		return ""
	}
	n, err := strconv.Atoi(strings.TrimSpace(answer))
	if err != nil || n < 1 || n > len(commands) {
		return ""
	}
	return commands[n-1]
}

// reviewSandboxTryLines runs /sandbox try as text: a trial, its review
// printed, then the user's answers until they allow, cancel, or run the
// command again. Where nothing can be asked it prints the review and allows
// nothing.
func reviewSandboxTryLines(ctx *replCommandContext, try *sandboxTry) []string {
	for {
		start := fmt.Sprintf("sandbox try: trial %d: running %s", len(try.trials)+1, commandLine(try.command))
		if with := try.ticked(); with > 0 {
			start += fmt.Sprintf(" with %d ticked %s", with, pluralWord(with, "item", "items"))
		}
		if err := ctx.replyLine(start); err != nil {
			return nil
		}
		dropped, err := try.trial(ctx.operationContext())
		for _, p := range dropped {
			_ = ctx.replyLine(fmt.Sprintf("sandbox try: unticked %s, since the rules now refuse it: %s", p.label(), p.refused))
		}
		if err != nil {
			return []string{"sandbox try: the trial did not run: " + err.Error()}
		}
		_ = ctx.replyLines(sandboxTryReviewLines(try))
		if ctx.readInput == nil {
			return []string{"sandbox try: nothing allowed; answering needs a terminal, and /sandbox allow adds an item by hand"}
		}
		if again, lines := answerSandboxTryLines(ctx, try); !again {
			return lines
		}
	}
}

// answerSandboxTryLines reads the user's answers to a review until one ends
// it: again reports that they asked for another trial, and lines are what to
// print when they allowed or cancelled.
func answerSandboxTryLines(ctx *replCommandContext, try *sandboxTry) (again bool, lines []string) {
	for {
		answer, err := ctx.readInput("sandbox try> ")
		if err != nil {
			return false, []string{"sandbox try: nothing allowed"}
		}
		verb, rest, _ := strings.Cut(strings.TrimSpace(answer), " ")
		switch verb {
		case "tick", "t":
			for _, field := range strings.FieldsFunc(rest, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
				n, err := strconv.Atoi(field)
				if err != nil {
					_ = ctx.replyLine("sandbox try: tick takes row numbers, such as tick 1,3")
					continue
				}
				if err := try.toggle(n - 1); err != nil {
					_ = ctx.replyLine("sandbox try: " + err.Error())
				}
			}
			_ = ctx.replyLines(sandboxTryRowLines(try, false))
		case "reads":
			n := try.tickReads()
			_ = ctx.replyLine(fmt.Sprintf("sandbox try: ticked %d %s", n, pluralWord(n, "read", "reads")))
			_ = ctx.replyLines(sandboxTryRowLines(try, false))
		case "try", "again":
			return true, nil
		case "save", "session":
			lines, err := try.allow(verb == "save")
			if err != nil {
				_ = ctx.replyLine("sandbox try: " + err.Error())
				continue
			}
			ctx.notifySandboxChanged()
			return false, lines
		case "output", "o":
			_ = ctx.replyLines(sandboxTryOutputText(try))
		case "list", "l":
			_ = ctx.replyLines(sandboxTryReviewLines(try))
		case "cancel", "q", "quit":
			return false, []string{"sandbox try: nothing allowed"}
		default:
			_ = ctx.replyLine(sandboxTryAnswers)
		}
	}
}

const sandboxTryAnswers = "answers: tick <n>[,<n>] · reads (tick every read) · try (again, with the ticked) · save (to the workspace profile) · session (this session only) · output · list · cancel"

// sandboxTryReviewLines is the review printed as text: the last trial's
// result and notes, then the rows, numbered for tick, with what each means.
func sandboxTryReviewLines(try *sandboxTry) []string {
	n := len(try.trials)
	lines := []string{"  " + try.trials[n-1].summary(n)}
	for _, note := range try.notes() {
		lines = append(lines, "  "+note)
	}
	if len(try.rows) == 0 {
		lines = append(lines, "  The sandbox denied the command nothing polly could see.")
	}
	lines = append(lines, sandboxTryRowLines(try, true)...)
	return append(lines, sandboxTryAnswers)
}

// sandboxTryRowLines lists the rows, with each one's explanation when
// details is set.
func sandboxTryRowLines(try *sandboxTry, details bool) []string {
	var lines []string
	for i, p := range try.rows {
		box := "[ ]"
		switch {
		case p.refused != "":
			box = " - "
		case p.ticked:
			box = "[x]"
		}
		line := fmt.Sprintf("  %d. %s %s", i+1, box, p.label())
		if badge := p.badge(); badge != "" {
			line += " · " + badge
		}
		lines = append(lines, line)
		if details {
			for _, detail := range p.details() {
				lines = append(lines, "       "+detail)
			}
		}
	}
	return lines
}

// sandboxTryOutputText is the end of the last trial's output.
func sandboxTryOutputText(try *sandboxTry) []string {
	output := try.trials[len(try.trials)-1].result.Output
	if output == "" {
		return []string{"sandbox try: the command printed nothing"}
	}
	lines := strings.Split(output, "\n")
	if len(lines) > sandboxTryOutputLines {
		lines = append([]string{fmt.Sprintf("… the last %d lines of %d:", sandboxTryOutputLines, len(lines))}, lines[len(lines)-sandboxTryOutputLines:]...)
	}
	return lines
}
