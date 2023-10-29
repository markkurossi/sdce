//
// Copyright (c) 2023 Markku Rossi
//
// All rights reserved.
//

package main

import (
	"crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"

	"github.com/markkurossi/mpc"
	"github.com/markkurossi/mpc/circuit"
	"github.com/markkurossi/mpc/compiler"
	"github.com/markkurossi/mpc/compiler/utils"
	"github.com/markkurossi/mpc/ot"
	"github.com/markkurossi/mpc/p2p"
)

var (
	params  *utils.Params
	oti     ot.OT
	mpcPort = ":9000"
	cmdPort = ":8080"
	states  = make(map[string]map[string][]byte)
	bo      = binary.BigEndian
)

func main() {
	evaluator := flag.Bool("e", false, "evaluator / garbler mode")
	fVerbose := flag.Bool("v", false, "verbose output")
	fDiagnostics := flag.Bool("d", false, "diagnostics output")
	flag.Parse()

	log.SetFlags(0)

	params = utils.NewParams()
	defer params.Close()

	oti = ot.NewCO()

	params.Verbose = *fVerbose
	params.Diagnostics = *fDiagnostics
	params.OptPruneGates = true

	fmt.Println("SDCE Node")

	var err error
	if *evaluator {
		err = evaluatorMode()
	} else {
		err = garblerMode()
	}
	if err != nil {
		log.Fatal(err)
	}
}

func evaluatorMode() error {
	listener, err := net.Listen("tcp", mpcPort)
	if err != nil {
		return err
	}
	log.Printf("Listening for MPC connections at %s", mpcPort)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		log.Printf("New MPC connection from %s", conn.RemoteAddr())
		go func(conn *p2p.Conn) {
			err := evaluatorLoop(conn)
			if err != nil {
				log.Printf("%s\n", err)
			}
		}(p2p.NewConn(conn))
	}
}

func evaluatorLoop(conn *p2p.Conn) error {
	defer conn.Close()

	inputSizes := make([][]int, 2)

	for {
		// Receive command.
		cmd, err := conn.ReceiveString()
		if err != nil {
			return err
		}
		command, ok := commands[cmd]
		if !ok {
			return fmt.Errorf("unknown command: %s", cmd)
		}

		// Receive session ID.
		id, err := conn.ReceiveString()
		if err != nil {
			return err
		}
		// XXX lock stats.
		state, ok := states[id]
		if !ok {
			state, err = newState()
			if err != nil {
				return err
			}
			states[id] = state
		}

		// Create input sizes.
		var sizes []int
		var inputs []string
		for _, key := range command.eInputs {
			input, ok := state[key]
			if !ok {
				return fmt.Errorf("state variable undefined: %s", key)
			}
			sizes = append(sizes, len(input)*8)
			inputs = append(inputs, fmt.Sprintf("0x%x", input))
		}
		inputSizes[1] = sizes

		// Evaluator sends first its input sizes.
		err = conn.SendInputSizes(sizes)
		if err != nil {
			return err
		}
		err = conn.Flush()
		if err != nil {
			return err
		}
		// Receive garbler sizes.
		sizes, err = conn.ReceiveInputSizes()
		if err != nil {
			return err
		}
		inputSizes[0] = sizes

		// Load circuit.
		circ, err := loadCircuit(command.file, inputSizes)
		if err != nil {
			return err
		}
		circ.PrintInputs(circuit.IDEvaluator, inputs)

		if circ.NumParties() != 2 {
			return fmt.Errorf("invalid circuit for 2-party MPC: %d parties",
				circ.NumParties())
		}
		input, err := circ.Inputs[1].Parse(inputs)
		if err != nil {
			return err
		}
		result, err := circuit.Evaluator(conn, oti, circ, input, true)
		if err != nil {
			return err
		}
		mpc.PrintResults(result, circ.Outputs)
	}
}

func garblerMode() error {
	// Create command listener.
	listener, err := net.Listen("tcp", cmdPort)
	if err != nil {
		return err
	}
	log.Printf("Listening for command connections at %s", cmdPort)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		log.Printf("New command connection from %s", conn.RemoteAddr())
		go func(conn *p2p.Conn) {
			err := garblerLoop(conn)
			if err != nil {
				if err != io.EOF {
					log.Printf("%s\n", err)
				}
			}
		}(p2p.NewConn(conn))
	}
}

func garblerLoop(conn *p2p.Conn) error {
	defer conn.Close()

	var done bool

	for !done {
		err := garblerCommand(conn)
		var result string
		if err != nil {
			done = true
			if err != io.EOF {
				result = err.Error()
				log.Printf("%s\n", result)
			}
		}
		err = conn.SendString(result)
		if err != nil {
			return err
		}
		err = conn.Flush()
		if err != nil {
			return err
		}
	}
	return nil
}

func garblerCommand(conn *p2p.Conn) error {
	argc, err := conn.ReceiveUint32()
	if err != nil {
		return err
	}
	var argv []string
	for i := 0; i < argc; i++ {
		arg, err := conn.ReceiveString()
		if err != nil {
			return err
		}
		argv = append(argv, arg)
	}
	fmt.Printf("argv: %v\n", argv)
	if len(argv) < 2 {
		return fmt.Errorf("invalid argv: %v", argv)
	}

	mpc, err := net.Dial("tcp", mpcPort)
	if err != nil {
		return err
	}
	mpcc := p2p.NewConn(mpc)

	// XXX locking for states.
	stateID := argv[0]
	state, ok := states[stateID]
	if !ok {
		state, err = initState(mpcc, stateID)
		if err != nil {
			return err
		}
		states[stateID] = state
	}

	var input [5]byte

	switch argv[0] {
	case "+":
		input[0] = '+'
	default:
		return fmt.Errorf("unknown command: %s", argv[0])
	}
	val, err := strconv.ParseInt(argv[1], 10, 32)
	if err != nil {
		return fmt.Errorf("invalid number argument '%v': %v", argv[1], err)
	}
	bo.PutUint32(input[1:], uint32(val))

	state["INPUT"] = input[:]

	return runCommand(mpcc, "event", stateID, state)
}

func newState() (map[string][]byte, error) {
	state := make(map[string][]byte)

	var key [16]byte
	_, err := rand.Read(key[:])
	if err != nil {
		return nil, err
	}
	state["KEY"] = key[:]

	return state, nil
}

func initState(conn *p2p.Conn, id string) (map[string][]byte, error) {
	state, err := newState()
	if err != nil {
		return nil, err
	}
	err = runCommand(conn, "init", id, state)
	if err != nil {
		return nil, err
	}
	return state, nil
}

func runCommand(conn *p2p.Conn, name, id string,
	state map[string][]byte) error {

	cmd, ok := commands[name]
	if !ok {
		return fmt.Errorf("unknown command: %s", name)
	}

	inputSizes := make([][]int, 2)

	var sizes []int
	var inputs []string

	for _, inputName := range cmd.gInputs {
		val, ok := state[inputName]
		if !ok {
			return fmt.Errorf("%s: input '%s' not defined", name, inputName)
		}
		sizes = append(sizes, len(val)*8)
		inputs = append(inputs, fmt.Sprintf("0x%x", val))
	}

	inputSizes[0] = sizes

	// Send command name and session ID.
	err := conn.SendString(name)
	if err != nil {
		return err
	}
	err = conn.SendString(id)
	if err != nil {
		return err
	}
	err = conn.Flush()
	if err != nil {
		return err
	}
	// Receive evaluator's input sizes.
	sizes, err = conn.ReceiveInputSizes()
	if err != nil {
		return err
	}
	inputSizes[1] = sizes

	// Send garbler's input sizes.
	err = conn.SendInputSizes(inputSizes[0])
	if err != nil {
		return err
	}
	err = conn.Flush()
	if err != nil {
		return err
	}

	// Load circuit.
	circ, err := loadCircuit(cmd.file, inputSizes)
	if err != nil {
		return err
	}
	circ.PrintInputs(circuit.IDGarbler, inputs)

	if circ.NumParties() != 2 {
		return fmt.Errorf("invalid circuit for 2-party MPC: %d parties",
			circ.NumParties())
	}
	input, err := circ.Inputs[0].Parse(inputs)
	if err != nil {
		return err
	}
	result, err := circuit.Garbler(conn, oti, circ, input, true)
	if err != nil {
		return err
	}
	mpc.PrintResults(result, circ.Outputs)
	values := mpc.Results(result, circ.Outputs)
	for idx, value := range values {
		if idx >= len(cmd.outputs) {
			fmt.Printf(" - extra%d=%v\n", idx, value)
			continue
		}
		switch v := value.(type) {
		case []byte:
			fmt.Printf(" - %s=%x\n", cmd.outputs[idx], v)
			state[cmd.outputs[idx]] = v
		default:
			fmt.Printf(" - %s=%v\n", cmd.outputs[idx], v)
		}
	}

	return nil
}

func loadCircuit(file string, inputSizes [][]int) (*circuit.Circuit, error) {
	circ, _, err := compiler.New(params).CompileFile(file, inputSizes)
	if err != nil {
		return nil, err
	}
	circ.AssignLevels()
	return circ, err
}

var commands = map[string]*struct {
	file    string
	gInputs []string
	eInputs []string
	outputs []string
}{
	"init": {
		file:    "init.mpcl",
		gInputs: []string{"KEY"},
		eInputs: []string{"KEY"},
		outputs: []string{"STATE", "NONCE"},
	},
	"event": {
		file:    "event.mpcl",
		gInputs: []string{"KEY", "NONCE", "STATE", "INPUT"},
		eInputs: []string{"KEY"},
		outputs: []string{"STATE", "NONCE", "SUCCESS"},
	},
}
