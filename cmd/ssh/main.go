package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"golang.org/x/crypto/ssh"

	expect "github.com/google/goexpect"
)

func main() {
	keyPath := "/home/ellorent/.ssh/id_ed25519"
	key, err := os.ReadFile(keyPath)
	if err != nil {
		log.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		log.Fatal(err)
	}
	// ssh config
	config := &ssh.ClientConfig{
		User:            "fedora",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
	}
	// connect ot ssh server
	address := fmt.Sprintf("%s:%s", os.Args[1], os.Args[2])
	conn, err := ssh.Dial("tcp", address, config)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	e, _, err := expect.SpawnSSH(conn, time.Minute, expect.Verbose(false))
	if err != nil {
		log.Fatal(err)
	}
	defer e.Close()

	max := time.Duration(0)
	for true {
		start := time.Now()
		_, err = e.ExpectBatch([]expect.Batcher{
			&expect.BExp{R: `\$`},
			&expect.BSnd{S: "date\n"},
			&expect.BExp{R: `.*`},
		}, 10*time.Minute)
		if err != nil {
			log.Fatal(err)
		}
		elapsed := time.Since(start)
		if elapsed > max {
			max = elapsed
		}
		fmt.Printf("latency: %s, max: %s\n", elapsed, max)
	}

	/*
		session, err := conn.NewSession()
		if err != nil {
			log.Fatal(err)
		}

		var sessionStdout, sessionStderr bytes.Buffer
		session.Stdout = &sessionStdout
		session.Stderr = &sessionStderr
		stdin, err := session.StdinPipe()
		if err != nil {
			log.Fatal(err)
		}

		if err := session.Shell(); err != nil {
			log.Fatal(err)
		}

		for true {
			fmt.Println("---> WriteString <---")
			if _, err := io.WriteString(stdin, "date\n"); err != nil {
				log.Fatal(fmt.Errorf("%s:%s:%v", sessionStdout.String(), sessionStderr.String(), err))
			}
			fmt.Println("---> Println <---")
			fmt.Println(sessionStdout.String())
			fmt.Println("---> Sleep <---")
			time.Sleep(time.Second)
		}
	*/

}
