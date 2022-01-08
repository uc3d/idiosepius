package main

import "fmt"

func main() {
	cmd := "N37647 G1 F3600 X105.069 Y117.244 E2413.19133*51"
	var cs byte
	for i := 0; cmd[i] != '*'; i++ {
		cs = cs ^ cmd[i]
	}
	cs &= 0xff // Defensive programming...
	fmt.Println(int(cs))
}
