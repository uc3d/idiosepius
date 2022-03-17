all:
	GOOS=linux GOARCH=arm64 go build -o idiosepius.linux.arm64
	GOOS=linux GOARCH=arm go build -o idiosepius.linux.arm
	GOOS=linux GOARCH=amd64 go build -o idiosepius.linux.amd64