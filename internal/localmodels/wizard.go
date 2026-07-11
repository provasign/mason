package localmodels

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Wizard is the interactive local-model experience: shows what's installed,
// what this machine can run, and downloads a pick on a single keypress.
// Returns the chosen model spec ("ollama:<tag>") or "" if the user skipped.
// When interactive is false it prints the table and returns immediately.
func Wizard(interactive bool) (string, error) {
	ram := SystemRAMGB()
	st := Detect()

	if !st.BinaryInstalled {
		fmt.Println("Ollama (the local model runtime) is not installed.")
		if !interactive {
			return "", fmt.Errorf("install it from https://ollama.com/download")
		}
		if !yes("Install it now?") {
			return "", nil
		}
		if err := InstallOllama(); err != nil {
			return "", err
		}
		st = Detect()
	}
	if st.BinaryInstalled && !st.ServerUp {
		fmt.Println("starting the Ollama server…")
		if err := StartServer(); err != nil {
			return "", err
		}
		st = Detect()
	}

	installed := st.InstalledSet()
	if ram > 0 {
		fmt.Printf("\nThis machine: %d GB memory\n", ram)
	} else {
		fmt.Println("\nThis machine: memory unknown — sizes shown, nothing filtered")
	}

	// One continuous numbered list: installed models select instantly,
	// catalog models download then select.
	num := 0
	if len(st.Installed) > 0 {
		fmt.Println("\nInstalled — press its number to use it:")
		for _, t := range st.Installed {
			num++
			mark := ""
			for _, c := range Catalog {
				if c.Tag == t && c.Blessed {
					mark = "   (recommended — measured at the engine ceiling)"
				}
			}
			fmt.Printf("  %d. ✓ %s%s\n", num, t, mark)
		}
	}
	nInstalled := num

	var runnable []Model
	var tooBig []Model
	for _, m := range Catalog {
		if installed[m.Tag] {
			continue
		}
		if m.Fits(ram) {
			runnable = append(runnable, m)
		} else {
			tooBig = append(tooBig, m)
		}
	}

	if len(runnable) > 0 {
		fmt.Println("\nAvailable to download (fits this machine):")
		for _, m := range runnable {
			num++
			star := ""
			if m.Blessed {
				star = " ★"
			}
			fmt.Printf("  %d. %-22s %4.1f GB download · needs %2d GB%s — %s\n",
				num, m.Tag, m.DownloadGB, m.MinRAMGB, star, m.Note)
		}
	}
	if len(tooBig) > 0 {
		fmt.Println("\nToo large for this machine:")
		for _, m := range tooBig {
			fmt.Printf("     %-22s needs %d GB memory\n", m.Tag, m.MinRAMGB)
		}
	}

	if !interactive || num == 0 {
		return "", nil
	}
	fmt.Print("\nPress a number to use or download (Enter to skip): ")
	line := readLine()
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > num {
		return "", nil
	}
	if n <= nInstalled {
		// Installed pick: instant switch, nothing to download.
		return "ollama:" + st.Installed[n-1], nil
	}
	pick := runnable[n-1-nInstalled]
	fmt.Printf("downloading %s (%.1f GB)…\n", pick.Tag, pick.DownloadGB)
	if err := Pull(pick.Tag); err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	fmt.Printf("✓ %s installed\n", pick.Tag)
	if yes("Use it now?") {
		return "ollama:" + pick.Tag, nil
	}
	return "", nil
}

func yes(prompt string) bool {
	fmt.Print(prompt + " [Y/n] ")
	ans := strings.ToLower(strings.TrimSpace(readLine()))
	return ans == "" || strings.HasPrefix(ans, "y")
}

func readLine() string {
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return line
}
