//
// Copyright (c) 2023 Markku Rossi
//
// All rights reserved.
//

package main

import (
	"flag"
	"fmt"
	"log"
	"net"

	"github.com/markkurossi/mpc/p2p"
)

var (
	cmdPort = ":8080"
)

func main() {
	session := flag.String("session", "", "session identifier")
	flag.Parse()
	log.SetFlags(0)

	fmt.Println("SDCE Client")

	if len(*session) == 0 {
		log.Fatal("no session ID specified")
	}

	nc, err := net.Dial("tcp", cmdPort)
	if err != nil {
		log.Fatal(err)
	}

	argv := []string{*session}
	argv = append(argv, flag.Args()...)

	err = doCommand(p2p.NewConn(nc), argv)
	if err != nil {
		log.Fatal(err)
	}
}

func doCommand(conn *p2p.Conn, argv []string) error {
	defer conn.Close()

	err := conn.SendUint32(len(argv))
	if err != nil {
		return err
	}
	for _, arg := range argv {
		err = conn.SendString(arg)
		if err != nil {
			return err
		}
	}
	err = conn.Flush()
	if err != nil {
		return err
	}
	result, err := conn.ReceiveString()
	if err != nil {
		return err
	}
	fmt.Printf(" => %s\n", result)
	return nil
}
