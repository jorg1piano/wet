binary_name := "wet"
install_dir := env("HOME") / ".my_tools/bin"

alias install := build

# Build and install the CLI
build:
    go build -o {{install_dir}}/{{binary_name}}

# Run without building
run *ARGS:
    go run . {{ARGS}}

# Clean the binary
clean:
    rm -f {{install_dir}}/{{binary_name}}

# Build and run worktree create
create *ARGS:
    go run . worktree create {{ARGS}}

# Build and run worktree list
list:
    go run . worktree list

# Tidy up go modules
tidy:
    go mod tidy
