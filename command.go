/*
 *  ZEUS - An Electrifying Build System
 *  Copyright (c) 2017 Philipp Mieden <dreadl0ck [at] protonmail [dot] ch>
 *
 *  This program is free software: you can redistribute it and/or modify
 *  it under the terms of the GNU General Public License as published by
 *  the Free Software Foundation, either version 3 of the License, or
 *  (at your option) any later version.
 *
 *  This program is distributed in the hope that it will be useful,
 *  but WITHOUT ANY WARRANTY; without even the implied warranty of
 *  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 *  GNU General Public License for more details.
 *
 *  You should have received a copy of the GNU General Public License
 *  along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/dreadl0ck/readline"
	"github.com/mgutz/ansi"
)

var (
	// ErrInvalidCommand means the command name is invalid, most likely empty.
	ErrInvalidCommand = errors.New("invalid command")

	// ErrInvalidArgumentType means the argument type does not match the expected type
	ErrInvalidArgumentType = errors.New("invalid argument type")

	// ErrInvalidArgumentLabel means the argument label does not match the expected label
	ErrInvalidArgumentLabel = errors.New("invalid argument label")

	// ErrInvalidDependency means the named dependency command does not exist
	ErrInvalidDependency = errors.New("invalid dependency")
)

type commandChain []*command

// command represents a parsed script in memory
type command struct {

	// the path where the script resides
	path string

	// commandName
	name string

	// arguments for the command
	// mapped labels to commandArg instances
	args map[string]*commandArg

	// parameters that can be set, for calling commands with arguments in commandChains
	params []string

	// short help text
	help string

	// manual text
	manual string

	// commandChain contains commands that will be executed before the command runs
	commandChain commandChain

	// async means the command will be spawned in the background
	async bool

	// completer for interactive shell
	PrefixCompleter *readline.PrefixCompleter

	// buildNumber
	buildNumber bool

	// if the command depends on other command(s)
	// add them here and they will be executed prior to execution of the current command
	// if their named output files to not exist
	dependencies []string

	// output file(s) of the command
	// if the file exists the command will not be executed
	outputs []string

	// if the command has been generated by a Zeusfile
	// the script that will be executed goes in here
	runCommand string
}

// Run executes the command
func (c *command) Run(args []string, async bool) error {

	// spawn async commands in a new goroutine
	if async {
		go func() {
			err := c.Run(args, false)
			if err != nil {
				Log.WithError(err).Error("failed to run command: " + c.name)
			}
		}()
		time.Sleep(50 * time.Millisecond)
		return nil
	}

	var (
		cLog  = Log.WithField("prefix", c.name)
		start = time.Now()
	)

	if len(c.outputs) != 0 {
		// check if named outputs exist
		for _, output := range c.outputs {

			Log.Debug("checking output: ", output)

			_, err := os.Stat(output)
			if err == nil {
				// file exists, skip it
				Log.WithFields(logrus.Fields{
					"commandName": c.name,
					"output":      output,
				}).Info("skipping command because its output exists")
				return nil
			}
		}
	}

	err := c.handleDependencies()
	if err != nil {
		return err
	}

	cLog.WithFields(logrus.Fields{
		"name":   c.name,
		"args":   args,
		"params": c.params,
	}).Debug("running command")

	// check if parameters are set on the command
	// in this case ignore the arguments from the commandline and pass the predefined ones
	if len(c.params) > 0 {
		Log.Debug("found predefined params: ", c.params)
		args = c.params
	}

	// execute build chain commands
	if len(c.commandChain) > 0 {
		for _, cmd := range c.commandChain {

			// dont pass the commandline args down the commandChain
			// if the following commands have required arguments they are set on the params fields
			err := cmd.Run([]string{}, cmd.async)
			if err != nil {
				cLog.WithError(err).Error("failed to execute " + cmd.name)
				return err
			}
		}
	}

	currentCommand++

	argBuffer, err := c.parseArguments(args)
	if err != nil {
		return err
	}

	cmd, script, err := c.createCommand(argBuffer)
	if err != nil {
		return err
	}

	cmd.Env = os.Environ()
	if !c.async {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
	}

	if c.buildNumber {
		projectDataMutex.Lock()
		projectData.BuildNumber++
		projectDataMutex.Unlock()
		projectData.update()
	}

	l.Print(cp.text)
	if c.async {
		l.Println(printPrompt() + "[" + strconv.Itoa(currentCommand) + "/" + strconv.Itoa(numCommands) + "] detaching " + cp.prompt + c.name + ansi.Reset)
	} else {
		l.Println(printPrompt() + "[" + strconv.Itoa(currentCommand) + "/" + strconv.Itoa(numCommands) + "] executing " + cp.prompt + c.name + ansi.Reset)
	}

	// lets go
	err = cmd.Start()
	if err != nil {
		cLog.WithError(err).Fatal("failed to start command: " + c.name)
	}

	// add to processMap
	var (
		id  = processID(randomString())
		pid = cmd.Process.Pid
	)
	Log.Debug("PID: ", pid)
	addProcess(id, c.name, cmd.Process, pid)

	// after command has finished running, remove from processMap
	defer deleteProcessByPID(pid)

	// wait for command to finish execution
	err = cmd.Wait()
	if err != nil {

		// when there are no globals, read the command script directly and print it with line numbers to stdout for easy debugging
		if script == "" {
			scriptBytes, err := ioutil.ReadFile(c.path)
			if err != nil {
				cLog.WithError(err).Error("failed to read script")
			}
			script = string(scriptBytes)
		}

		if conf.DumpScriptOnError {
			dumpScript(script, err)
		}

		return err
	}

	if c.async {
		// add to process map PID +1
		Log.Debug("detached PID: ", pid+1)
		addProcess(id, c.name, nil, pid+1)

		func() {
			for {

				// check if detached process is still alive
				// If  sig is 0, then no signal is sent, but error checking is still performed
				// this can be used to check for the existence of a process ID or process group ID
				err := exec.Command("kill", "-0", strconv.Itoa(pid+1)).Run()
				if err != nil {
					Log.Debug("detached process with PID " + strconv.Itoa(pid+1) + "exited")
					deleteProcessByPID(pid + 1)
					return
				}

				time.Sleep(2 * time.Second)
			}
		}()
	} else {
		// print stats
		l.Println(
			printPrompt()+"["+strconv.Itoa(currentCommand)+"/"+strconv.Itoa(numCommands)+"] finished "+cp.prompt+c.name+cp.text+" in"+cp.prompt,
			time.Now().Sub(start),
			ansi.Reset,
		)
	}

	return nil
}

func (c *command) handleDependencies() error {

	// check if there are dependencies for the current command
	if len(c.dependencies) != 0 {

		for _, dep := range c.dependencies {

			numCommands++

			var (
				cmd *command
				ok  bool
				err error
			)

			// handle args
			fields := strings.Fields(dep)
			if len(fields) == 0 {
				Log.Error("empty fields")
				return ErrInvalidDependency
			}

			// look up command name
			if cmd, ok = commands[fields[0]]; !ok {
				return ErrInvalidDependency
			}

			if len(cmd.outputs) != 0 {

				var outputMissing bool

				// check if all named outputs exist
				for _, output := range cmd.outputs {
					_, err := os.Stat(output)
					if err != nil {
						outputMissing = true
					}
				}

				// there is at least one output missing
				// execute the command
				if outputMissing {

					// pass args if there are any
					if len(fields) > 1 {
						err = cmd.Run(fields[1:], c.async)
					} else {
						err = cmd.Run([]string{}, c.async)
					}
					if err != nil {
						Log.WithError(err).Error("failed to execute dependency command: " + cmd.name)
						return err
					}
				}
			}
		}
	}

	return nil
}

func (c *command) parseArguments(args []string) (string, error) {

	Log.Debug("parsing args: ", args, " cmd: ", c.name)

	var (
		argStr = strings.Join(args, " ")
		argBuf bytes.Buffer
	)

	// parse args
	for _, val := range args {

		// handle argument labels
		if strings.Contains(val, "=") {

			var (
				cmdArg *commandArg
				ok     bool
			)

			argSlice := strings.Split(val, "=")
			if len(argSlice) != 2 {
				return "", errors.New("invalid argument: " + val)
			}

			if cmdArg, ok = c.args[argSlice[0]]; !ok {
				Log.Error("invalid label: " + argSlice[0])
				return "", ErrInvalidArgumentLabel
			}

			if !validArgType(argSlice[1], cmdArg.argType) {
				Log.WithError(ErrInvalidArgumentType).WithFields(logrus.Fields{
					"value": argSlice[1],
					"label": cmdArg.name,
				}).Error("expected type: ", cmdArg.argType.String())
				return "", ErrInvalidArgumentType
			}

			if strings.Count(argStr, cmdArg.name+"=") > 1 {
				return "", errors.New("argument label appeared more than once: " + cmdArg.name)
			}

			c.args[argSlice[0]].value = argSlice[1]
		} else {
			return "", errors.New("invalid argument: " + val)
		}
	}

	for _, arg := range c.args {
		if arg.value == "" {
			if arg.optional {
				if arg.defaultValue != "" {
					// default value has been set
					argBuf.WriteString(arg.name + "=" + strings.TrimSpace(arg.defaultValue) + "\n")
				} else {
					// init empty optionals with default value for their type
					argBuf.WriteString(arg.name + "=" + getDefaultValue(arg) + "\n")
				}
			} else {
				// empty value and not optional - error
				return "", errors.New("missing argument: " + arg.name)
			}
		} else {
			// write value into buffer
			argBuf.WriteString(arg.name + "=" + arg.value + "\n")
		}
	}

	// flush arg values
	for _, arg := range c.args {
		arg.value = ""
	}

	return argBuf.String(), nil
}

func (c *command) createCommand(argBuffer string) (cmd *exec.Cmd, script string, err error) {

	var shellCommand []string

	if c.async {
		shellCommand = append(shellCommand, []string{"screen", "-L", "-S", c.name, "-dm"}...)
	}

	if conf.StopOnError {
		shellCommand = append(shellCommand, []string{p.interpreter, "-e", "-c"}...)
	} else {
		shellCommand = append(shellCommand, []string{p.interpreter, "-c"}...)
	}

	if c.runCommand != "" {
		script += string(globalsContent) + argBuffer + "\n" + c.runCommand
		shellCommand = append(shellCommand, script)
		Log.Debug("shellCommand: ", shellCommand)
		cmd = exec.Command(shellCommand[0], shellCommand[1:]...)
	} else {
		// make script executable
		err := os.Chmod(c.path, 0700)
		if err != nil {
			Log.Error("failed to make script executable")
			return nil, "", err
		}

		// read the contents of this commands script
		target, err := ioutil.ReadFile(c.path)
		if err != nil {
			l.Fatal(err)
		}

		// prepend projectGlobals if not empty
		if len(globalsContent) > 0 {

			// add the globals, append argument buffer and then append script contents
			script = string(append(append(globalsContent, []byte(argBuffer)...), target...))
			shellCommand = append(shellCommand, script)
			Log.Debug("shellCommand: ", shellCommand)
			cmd = exec.Command(shellCommand[0], shellCommand[1:]...)
		} else {

			// add argument buffer and then append script contents
			script = string(append([]byte(argBuffer), target...))
			shellCommand = append(shellCommand, script)
			Log.Debug("shellCommand: ", shellCommand)
			cmd = exec.Command(shellCommand[0], shellCommand[1:]...)
		}
	}

	if conf.Debug {
		printScript(script, c.name)
	}

	return
}

/*
 *	Utils
 */

func getDefaultValue(arg *commandArg) string {
	switch arg.argType {
	case reflect.String:
		return ""
	case reflect.Int:
		return "0"
	case reflect.Bool:
		return "false"
	case reflect.Float64:
		return "0.0"
	default:
		return "unknown type"
	}
}

// addCommand parses the script at path, adds it to the commandMap and sets up the shell completer
// if force is set to true the command will parsed again even when it already has been parsed
func addCommand(path string, force bool) error {

	// check if command is currently being parsed
	if p.JobExists(path) {
		Log.Warn("addCommand: JOB EXISTS: ", path)
		p.WaitForJob(path)
		return nil
	}

	var (
		cLog = Log.WithField("prefix", "addCommand")

		// create parse job
		job = p.AddJob(path, false)
	)

	if !force {

		commandName := strings.TrimSuffix(filepath.Base(path), f.fileExtension)
		commandMutex.Lock()
		_, ok := commands[commandName]
		commandMutex.Unlock()

		if ok {
			return nil
		}
	}

	// create new command instance
	cmd, err := job.newCommand(path)
	if err != nil {
		return err
	}

	// job done
	p.RemoveJob(job)

	// add to the completer
	// when being forced the command has already been parsed
	// so we dont need to add it again
	if !force {
		completerLock.Lock()
		completer.Children = append(completer.Children, cmd.PrefixCompleter)
		completerLock.Unlock()
	}

	commandMutex.Lock()
	// add to command map
	commands[cmd.name] = cmd
	commandMutex.Unlock()

	cLog.Debug("added " + ansi.Red + cmd.name + ansi.Reset + " to the command map")

	return nil
}

// validate arg string and return the validatedArgs as map
func validateArgs(args string) (validatedArgs map[string]*commandArg, err error) {

	// init map
	validatedArgs = make(map[string]*commandArg, 0)

	// empty string - empty args
	if len(args) == 0 {
		return
	}

	// parse arg string
	// split by commas
	for i, s := range strings.Split(args, ",") {

		if len(s) == 0 {
			return nil, errors.New("found empty argument at index: " + strconv.Itoa(i))
		}

		var (
			k            reflect.Kind
			slice        = strings.Split(s, ":")
			opt          bool
			defaultValue string
		)

		if len(slice) == 2 {

			// argument name may contain leading whitespace - trim it
			var argumentName = strings.TrimSpace(slice[0])

			// check for duplicate argument names
			if a, ok := validatedArgs[argumentName]; ok {
				Log.Error("argument label ", a.name, " was used twice")
				return nil, ErrDuplicateArgumentNames
			}

			// check if there's a default value set
			defaultValSlice := strings.Split(slice[1], "=")
			if len(defaultValSlice) > 1 {
				if !strings.Contains(slice[1], "?") {
					return nil, errors.New("default values for mandatory arguments are not allowed: " + s + ", at index: " + strconv.Itoa(i))
				}
				slice[1] = strings.TrimSpace(defaultValSlice[0])
				defaultValue = defaultValSlice[1]
			}

			// check if its an optional arg
			if strings.HasSuffix(slice[1], "?") {
				slice[1] = strings.TrimSuffix(slice[1], "?")
				opt = true
			}

			// check if its a valid argType and set reflect.Kind
			switch slice[1] {
			case argTypeBool:
				k = reflect.Bool
			case argTypeFloat:
				k = reflect.Float64
			case argTypeString:
				k = reflect.String
			case argTypeInt:
				k = reflect.Int
			default:
				return nil, errors.New("invalid or missing argument type: " + s)
			}

			// add to validatedArgs
			validatedArgs[argumentName] = &commandArg{
				name:         argumentName,
				argType:      k,
				optional:     opt,
				defaultValue: defaultValue,
			}
		} else {
			return nil, errors.New("invalid argument declaration: " + s)
		}
	}

	return
}

// newCommand creates a new command instance for the script at path
// a parseJob will be created
func (job *parseJob) newCommand(path string) (*command, error) {

	var (
		cLog = Log.WithField("prefix", "newCommand")
	)

	// parse the script
	d, err := p.parseScript(path, job)
	if err != nil {
		if !job.silent {
			cLog.WithFields(logrus.Fields{
				"path": path,
			}).Debug("Parse error")
		}
		return nil, err
	}

	// assemble commands args
	args, err := validateArgs(d.Args)
	if err != nil {
		return nil, err
	}

	chain, err := parseCommandChain(d.Chain)
	if err != nil {
		return nil, err
	}

	// get build chain
	commandChain, err := job.getCommandChain(chain, nil)
	if err != nil {
		return nil, err
	}

	// get name for command
	name := strings.TrimSuffix(strings.TrimPrefix(path, zeusDir+"/"), f.fileExtension)
	if name == "" {
		return nil, ErrInvalidCommand
	}

	return &command{
		path:         path,
		name:         name,
		args:         args,
		manual:       d.Manual,
		help:         d.Help,
		commandChain: commandChain,
		PrefixCompleter: readline.PcItem(name,
			readline.PcItemDynamic(func(path string) (res []string) {
				for _, a := range args {
					if !strings.Contains(path, a.name+"=") {
						res = append(res, a.name+"=")
					}
				}
				return
			}),
		),
		buildNumber:  d.BuildNumber,
		dependencies: d.Dependencies,
		outputs:      d.Outputs,
		async:        d.Async,
	}, nil
}

// assemble a commandChain with a list of parsed commands and their arguments
func (job *parseJob) getCommandChain(parsedCommands [][]string, zeusfile *Zeusfile) (commandChain commandChain, err error) {

	var cLog = Log.WithFields(logrus.Fields{
		"prefix":         "getCommandChain",
		"parsedCommands": parsedCommands,
	})

	cLog.Debug("creating commandChain, job.commands: ", job.commands)

	// empty commandChain is OK
	for _, args := range parsedCommands {

		var count int

		// check if there are repetitive targets in the chain - this is not allowed to prevent cycles
		for _, c := range job.commands {

			// check if the key (commandName) is already there
			if c[0] == args[0] {
				count++
			}
		}

		if count > p.recursionDepth {
			cLog.WithFields(logrus.Fields{
				"count":          count,
				"path":           job.path,
				"parsedCommands": parsedCommands,
				"job.commands":   job.commands,
			}).Error("CYCLE DETECTED! -> ", args[0], " appeared more than ", p.recursionDepth, " times - thats invalid.")
			cleanup()
			os.Exit(1)
		}

		job.commands = append(job.commands, args)

		var jobPath = zeusDir + "/" + args[0] + f.fileExtension
		if zeusfile != nil {
			jobPath = "zeusfile." + args[0]
		}

		// check if command has already been parsed
		commandMutex.Lock()
		cmd, ok := commands[args[0]]
		commandMutex.Unlock()

		if !ok {

			// check if command is currently being parsed
			if p.JobExists(jobPath) {
				Log.Warn("getCommandChain: JOB EXISTS: ", jobPath)
				p.WaitForJob(jobPath)

				// now the command is there
				commandMutex.Lock()
				cmd, ok = commands[args[0]]
				commandMutex.Unlock()
			} else {
				if zeusfile != nil {

					d := zeusfile.Commands[args[0]]
					if d == nil {
						return nil, errors.New("invalid command in commandChain: " + args[0])
					}

					// assemble commands args
					arguments, err := validateArgs(d.Args)
					if err != nil {
						return commandChain, err
					}

					// create command
					cmd = &command{
						path:            "",
						name:            args[0],
						args:            arguments,
						manual:          d.Manual,
						help:            d.Help,
						commandChain:    commandChain,
						PrefixCompleter: readline.PcItem(args[0]),
						buildNumber:     d.BuildNumber,
						dependencies:    d.Dependencies,
						outputs:         d.Outputs,
						runCommand:      d.Run,
						async:           d.Async,
					}
				} else {
					// add new command
					cmd, err = job.newCommand(zeusDir + "/" + args[0] + f.fileExtension)
					if err != nil {
						if !job.silent {
							cLog.WithError(err).Debug("failed to create command")
						}
						return commandChain, err
					}
				}
				commandMutex.Lock()

				// add the completer
				completer.Children = append(completer.Children, cmd.PrefixCompleter)

				// add to command map
				commands[args[0]] = cmd

				commandMutex.Unlock()

				cLog.Debug("added " + ansi.Red + cmd.name + ansi.Reset + " to the command map")
			}
		}

		cLog.Debug("adding command to build chain: ", args)

		// this command has argument parameters in its commandChain
		// set them on the command
		if len(args) > 1 {

			cLog.WithFields(logrus.Fields{
				"command": args[0],
				"params":  args[1:],
			}).Debug("setting parameters")

			// creating a hard copy of the struct here,
			// otherwise params would be set for every execution of the command
			cmd = &command{
				name:            cmd.name,
				path:            cmd.path,
				params:          args[1:],
				args:            cmd.args,
				manual:          cmd.manual,
				help:            cmd.help,
				commandChain:    cmd.commandChain,
				PrefixCompleter: cmd.PrefixCompleter,
				buildNumber:     cmd.buildNumber,
			}
		}

		// append command to build chain
		commandChain = append(commandChain, cmd)
	}
	return
}

// parse and execute a given commandChain string
func executeCommandChain(chain string) {

	var (
		cLog = Log.WithField("prefix", "executeCommandChain")
		job  = p.AddJob(chain, false)
	)

	defer p.RemoveJob(job)

	commandList, err := parseCommandChain(chain)
	if err != nil {
		cLog.WithError(err).Error("failed to parse command chain")
		return
	}

	commandChain, err := job.getCommandChain(commandList, nil)
	if err != nil {
		cLog.WithError(err).Error("failed to get command chain")
		return
	}

	numCommands = countCommandChain(commandChain)

	for _, c := range commandChain {
		err := c.Run([]string{}, c.async)
		if err != nil {
			cLog.WithError(err).Error("failed to execute " + c.name)
		}
	}

	// reset counters
	numCommands = 0
	currentCommand = 0
}

// walk all scripts in the zeus dir and setup commandMap and globals
func findCommands() {

	var (
		cLog    = Log.WithField("prefix", "findCommands")
		start   = time.Now()
		scripts []string
		// keep track of scripts that couldn't be parsed
		parseErrors      = make(map[string]error, 0)
		parseErrorsMutex = &sync.Mutex{}
	)

	// walk zeus directory and initialize scripts
	err := filepath.Walk(zeusDir, func(path string, info os.FileInfo, err error) error {

		// ignore self
		if path != zeusDir {

			// ignore sub directories
			if info.IsDir() {
				return filepath.SkipDir
			}

			// check if its a valid script
			if strings.HasSuffix(path, f.fileExtension) {

				// check for globals script
				// the globals script wont be parsed for zeus header fields
				if strings.HasPrefix(strings.TrimPrefix(path, zeusDir+"/"), "globals") {

					g, err := ioutil.ReadFile(zeusDir + "/globals.sh")
					if err != nil {
						l.Fatal(err)
					}

					// append a newline to prevent parse errors
					globalsContent = append(g, []byte("\n\n")...)
					return nil
				}

				scripts = append(scripts, path)
			}
		}

		return nil
	})
	if err != nil {
		cLog.WithError(err).Fatal("failed to walk zeus directory")
	}

	if len(scripts) > 10 {

		Log.Debug("parsing scripts asynchronously")

		// Asynchronous approach

		var wg sync.WaitGroup
		wg.Add(1)

		// first half
		go func() {
			for _, path := range scripts[:len(scripts)/2] {
				err := addCommand(path, false)
				if err != nil {
					Log.WithError(err).Debug("failed to add command")
					parseErrorsMutex.Lock()
					parseErrors[path] = err
					parseErrorsMutex.Unlock()
				}
			}
			wg.Done()
		}()

		wg.Add(1)

		// second half
		go func() {
			for _, path := range scripts[len(scripts)/2:] {
				err := addCommand(path, false)
				if err != nil {
					Log.WithError(err).Debug("failed to add command")
					parseErrorsMutex.Lock()
					parseErrors[path] = err
					parseErrorsMutex.Unlock()
				}
			}
			wg.Done()
		}()

		wg.Wait()
	} else {

		Log.Debug("parsing scripts sequentially")

		// sequential approach
		for _, path := range scripts {
			err := addCommand(path, false)
			if err != nil {
				Log.WithError(err).Debug("failed to add command")
				parseErrorsMutex.Lock()
				parseErrors[path] = err
				parseErrorsMutex.Unlock()
			}
		}
	}

	// print parse errors
	for path, err := range parseErrors {
		Log.WithError(err).Error("failed to parse: ", path)
	}
	// add a newline when there were parse errors
	if len(parseErrors) > 0 {
		println()
	}

	// only print info when using the interactive shell
	if len(os.Args) == 1 {
		l.Println(cp.text+"initialized "+cp.prompt, len(commands), cp.text+" commands in: "+cp.prompt, time.Now().Sub(start), ansi.Reset+"\n")
	}

	// check if custom command conflicts with builtin name
	for _, name := range builtins {
		if _, ok := commands[name]; ok {
			cLog.Error("command ", name, " conflicts with a builtin command. Please choose a different name.")
		}
	}

	var commandCompletions []readline.PrefixCompleterInterface
	for _, c := range commands {
		commandCompletions = append(commandCompletions, readline.PcItem(c.name))
	}

	// add all commands to the completer for the help page
	for _, c := range completer.Children {
		if string(c.GetName()) == "help " {
			c.SetChildren(commandCompletions)
		}
	}
}

func (c *command) dump() {
	fmt.Println("-----------------------------------------------------------")
	fmt.Println("name:", c.name)
	fmt.Println("path:", c.path)
	for _, arg := range c.args {
		fmt.Println(arg.name, "~>", arg.argType.String(), "optional:", arg.optional)
	}
	fmt.Println("params:", c.params)
	fmt.Println("help:", c.help)
	fmt.Println("manual:", c.manual)
	fmt.Println("commandChain:")
	for i, cmd := range c.commandChain {
		fmt.Println("command" + strconv.Itoa(i) + ":")
		cmd.dump()
	}
	fmt.Println("buildNumber:", c.buildNumber)
	fmt.Println("async:", c.async)
	fmt.Println("dependencies:", c.dependencies)
	fmt.Println("outputs:", c.outputs)
	fmt.Println("runCommand:", c.runCommand)
	fmt.Println("-----------------------------------------------------------")
}
