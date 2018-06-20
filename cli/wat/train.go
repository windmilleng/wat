package wat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	pb "gopkg.in/cheggaaa/pb.v1"

	isatty "github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

const trainRecencyCutoff = time.Hour
const trainTTL = 48 * time.Hour

// Only fuzz files that match this suffix.
// TODO(nick): Will we need to make this configurable?
var fuzzSuffixes = []string{
	// TODO(nick): Right now, we add comments to the file that
	// will only work in JS and Go. If we add other languages, we will
	// need to make the fuzz step more configurable.
	".go",
	".js",
}

var matchFalse = regexp.MustCompile("\\bfalse\\b")
var matchZero = regexp.MustCompile("\\b0\\b")

var trainCmd = &cobra.Command{
	Use:   "train",
	Short: "Train a model to make decisions on what to test",
	Run:   train,
}

func train(cmd *cobra.Command, args []string) {
	ctx, cancel := context.WithTimeout(context.Background(), CmdTimeout)
	defer cancel()

	ws, err := GetOrInitWatWorkspace()
	if err != nil {
		ws.Fatal("GetWatWorkspace", err)
	}

	cmds, err := populateAt(ctx, ws)
	if err != nil {
		ws.Fatal("List", err)
	}

	logs, err := Train(ctx, ws, cmds, 0 /* always fresh */)
	if err != nil {
		ws.Fatal("Train", err)
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(logs)
	if err != nil {
		ws.Fatal("Encode", err)
	}
}

// Gets training data.
//
// If sufficiently fresh training data lives on disk, return that data.
// Otherwise, generate new training data and write it to disk.
func Train(ctx context.Context, ws WatWorkspace, cmds []WatCommand, ttl time.Duration) ([]CommandLogGroup, error) {
	if ttl > 0 {
		info, err := ws.Stat(fnameCmdLog)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}

		// TODO(nick): This will do training if the user hasn't run wat for a while.
		// It might make sense to be more aggressive about this, e.g., run training
		// if the user hasn't explicitly trained for a while.
		exists := err == nil
		if exists && time.Since(info.ModTime()) < ttl {
			logs, err := ReadCmdLogGroups(ws)
			if err != nil {
				return nil, err
			}
			return logs, nil
		}
	}

	result, err := trainAt(ctx, ws, cmds)
	if err != nil {
		return nil, err
	}

	err = CmdLogGroupsToFile(ws, result)
	if err != nil {
		return nil, err
	}
	return result, nil
}

type LogSource int

const (
	_ = iota

	// An edit made by the user
	LogSourceUser LogSource = iota

	// An made-up command-log used to bootstrap training,
	// so that we have interesting data to work with before the
	// user runs any commands.
	LogSourceBootstrap

	// An edit automatically generated by a fuzzer
	LogSourceFuzz

	// Logs generated when the trainer runs the commands
	// in the workspace for the first time.
	LogSourceTrainInit
)

// All the commands that ran at a particular state of the workspace, grouped together.
type CommandLogGroup struct {
	Logs    []CommandLog
	Context LogContext
}

func newCommandLogGroup(ctx LogContext) *CommandLogGroup {
	return &CommandLogGroup{Context: ctx}
}

func (g *CommandLogGroup) Add(l CommandLog) {
	g.Logs = append(g.Logs, l)
}

type LogContext struct {
	// watRoot-relative paths of files that have been recently edited.
	// The definition of "recent" is deliberately fuzzy and might change.
	RecentEdits []string

	StartTime time.Time
	Source    LogSource
}

type CommandLog struct {
	// The Command field of WatCommand
	Command string

	Success  bool
	Duration time.Duration
}

func trainAt(ctx context.Context, ws WatWorkspace, cmds []WatCommand) ([]CommandLogGroup, error) {
	if isatty.IsTerminal(os.Stdout.Fd()) {
		fmt.Fprintln(os.Stderr, "Beginning training...type <Enter> or <Esc> to interrupt")

		var cancel func()
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()

		go func() {
			waitOnInterruptChar(ctx, []rune{AsciiEnter, AsciiLineFeed, AsciiEsc})
			cancel()
		}()
	}

	files, err := ws.WalkRoot()
	if err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(fileInfos(files)))

	result := make([]CommandLogGroup, 0, len(cmds))

	// Run all commands in the current workspace.
	recentEdit := ""
	if len(files) > 0 && time.Since(files[0].modTime) < trainRecencyCutoff {
		recentEdit = files[0].name
	}
	g, err := runInitGroup(ctx, cmds, ws.Root(), recentEdit)
	if err != nil {
		return nil, err
	}
	if len(g.Logs) != 0 {
		result = append(result, g)
	}

	// Fuzz each file and run all commands. This may take a long time. We expect
	// the user to cancel or time to run out before we finish, so we fuzz the files
	// in order of recent edits, and handle timeout/cancel gracefully.
	for _, f := range files {
		if ctx.Err() != nil {
			break
		}

		if !shouldFuzzFile(f.name) {
			continue
		}

		g, err := fuzzAndRun(ctx, cmds, ws.Root(), f.name)
		if err != nil {
			return nil, err
		}

		if len(g.Logs) != 0 {
			result = append(result, g)
		}
	}

	return result, nil
}

// Create an "init" group that runs all the commands in the current workspace.
func runInitGroup(ctx context.Context, cmds []WatCommand, root string, recentEdit string) (CommandLogGroup, error) {
	fmt.Fprintln(os.Stderr, "Running all tests in the current workspace")
	return runCmdsWithProgress(ctx, cmds, root, LogContext{
		StartTime:   time.Now(),
		Source:      LogSourceTrainInit,
		RecentEdits: []string{recentEdit},
	})
}

func runCmdsWithProgress(ctx context.Context, cmds []WatCommand, root string, logCtx LogContext) (CommandLogGroup, error) {
	g := CommandLogGroup{
		Context: logCtx,
	}
	bar := pb.New(len(cmds))
	bar.Output = os.Stderr
	bar.Start()
	defer bar.FinishPrint("")

	for i, cmd := range cmds {
		l, err := runCmdAndLog(ctx, root, cmd, ioutil.Discard, ioutil.Discard)
		if err != nil {
			if err == context.DeadlineExceeded || err == context.Canceled {
				break
			}
			return CommandLogGroup{}, err
		}
		g.Logs = append(g.Logs, l)
		bar.Set(i + 1)
	}

	return g, nil
}

func shouldFuzzFile(fileToFuzz string) bool {
	for _, suffix := range fuzzSuffixes {
		if strings.HasSuffix(fileToFuzz, suffix) {
			return true
		}
	}
	return false
}

// A dumb mutation: replace false with true and 0 with 1.
func fuzz(contents []byte) []byte {
	contents = matchFalse.ReplaceAll(contents, []byte("true"))
	contents = matchZero.ReplaceAll(contents, []byte("1"))
	return contents
}

// Make a random edit to a file and run all tests in the workspace.
func fuzzAndRun(ctx context.Context, cmds []WatCommand, root, fileToFuzz string) (CommandLogGroup, error) {
	absPath := filepath.Join(root, fileToFuzz)
	oldContents, err := ioutil.ReadFile(absPath)
	if err != nil {
		return CommandLogGroup{}, err
	}

	newContents := fuzz(oldContents)
	if bytes.Equal(newContents, oldContents) {
		// if fuzzing does nothing, don't bother.
		return CommandLogGroup{}, nil
	}

	// TODO(nick): right now this only works in JS and Go
	newContents = append(newContents,
		[]byte("\n// Modified by WAT fuzzer (https://github.com/windmilleng/wat)")...)

	// We know the file exists, so we expect that this file mode will be ignored
	mode := permFile

	// It's super important that we clean up the file, even if the user
	// tries to kill the process.
	tearDown := createCleanup(func() {
		ioutil.WriteFile(absPath, oldContents, mode)
	})
	defer tearDown()

	err = ioutil.WriteFile(absPath, newContents, mode)
	if err != nil {
		return CommandLogGroup{}, err
	}

	_, _ = fmt.Fprintf(os.Stderr, "Fuzzing %q and running all tests\n", fileToFuzz)
	return runCmdsWithProgress(ctx, cmds, root, LogContext{
		StartTime:   time.Now(),
		Source:      LogSourceFuzz,
		RecentEdits: []string{fileToFuzz},
	})
}
