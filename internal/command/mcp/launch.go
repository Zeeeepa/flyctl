package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/google/shlex"
	"github.com/spf13/cobra"
	"github.com/superfly/flyctl/internal/command"
	"github.com/superfly/flyctl/internal/flag"
	"github.com/superfly/flyctl/internal/logger"
)

// MCPConfig represents the structure of the JSON file
type MCPConfig struct {
	MCPServers map[string]MCPServer `json:"mcpServers"`
}

// Server represents a server configuration in the JSON file
type MCPServer struct {
	Args    []string `json:"args"`
	Command string   `json:"command"`
}

func NewLaunch() *cobra.Command {
	const (
		short = "[experimental] Launch an MCP stdio server"
		long  = short + "\n"
		usage = "launch command"
	)
	cmd := command.New(usage, short, long, runLaunch)
	cmd.Args = cobra.MaximumNArgs(1)

	flag.Add(cmd,
		flag.String{
			Name:        "name",
			Description: "Name to use for the MCP server in the MCP client configuration",
		},
		flag.String{
			Name:        "user",
			Description: "User to authenticate with",
		},
		flag.String{
			Name:        "password",
			Description: "Password to authenticate with",
		},
		flag.Bool{
			Name:        "bearer-token",
			Description: "Use bearer token for authentication",
			Default:     true,
		},
		flag.Bool{
			Name:        "flycast",
			Description: "Use wireguard and flycast for access",
		},
		flag.Bool{
			Name:        "inspector",
			Description: "Launch MCP inspector: a developer tool for testing and debugging MCP servers",
			Default:     false,
			Shorthand:   "i",
		},
		flag.StringArray{
			Name:        "config",
			Description: "Path to the MCP client configuration file (can be specified multiple times)",
		},
		flag.String{
			Name:        "auto-stop",
			Description: "Automatically suspend the app after a period of inactivity. Valid values are 'off', 'stop', and 'suspend",
			Default:     "stop",
		},
	)

	for client, name := range McpClients {
		flag.Add(cmd,
			flag.Bool{
				Name:        client,
				Description: "Add MCP server to the " + name + " client configuration",
			},
		)
	}

	return cmd
}

func runLaunch(ctx context.Context) error {
	log := logger.FromContext(ctx)

	flyctl, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to find executable: %w", err)
	}

	// Parse the command
	command := flag.FirstArg(ctx)
	cmdParts, err := shlex.Split(command)
	if err != nil {
		return fmt.Errorf("failed to parse command: %w", err)
	} else if len(cmdParts) == 0 {
		return fmt.Errorf("missing command to run")
	}

	// determine the name of the MCP server
	name := flag.GetString(ctx, "name")
	if name == "" {
		name = "fly-mcp"

		ingoreWords := []string{"npx", "uv", "-y", "--yes"}

		for _, w := range cmdParts {
			if !slices.Contains(ingoreWords, w) {
				re := regexp.MustCompile(`[-\w]+`)
				split := re.FindAllString(w, -1)

				if len(split) > 0 {
					name = split[len(split)-1]
					break
				}
			}
		}
	}

	// Create a temporary directory
	tempDir, err := os.MkdirTemp("", name)
	if err != nil {
		return fmt.Errorf("failed to create temporary directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	log.Debugf("Created temporary directory: %s\n", tempDir)

	if err := os.Chdir(tempDir); err != nil {
		return fmt.Errorf("failed to change to temporary directory: %w", err)
	}

	// Build the Dockerfile
	jsonData, err := json.Marshal(cmdParts)
	if err != nil {
		return fmt.Errorf("failed to marshal command parts to JSON: %w", err)
	}

	dockerfile := []string{
		"FROM flyio/mcp",
		"CMD " + string(jsonData),
	}

	dockerfileContent := strings.Join(dockerfile, "\n") + "\n"

	if err := os.WriteFile(filepath.Join(tempDir, "Dockerfile"), []byte(dockerfileContent), 0644); err != nil {
		return fmt.Errorf("failed to create Dockerfile: %w", err)
	}

	log.Debug("Created Dockerfile")

	args := []string{"launch", "--yes", "--no-deploy"}

	if flycast := flag.GetBool(ctx, "flycast"); flycast {
		args = append(args, "--flycast")
	}

	if autoStop := flag.GetString(ctx, "auto-stop"); autoStop != "" {
		args = append(args, "--auto-stop", autoStop)
	}

	// Run fly launch, but don't deploy
	cmd := exec.Command(flyctl, args...)
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to run 'fly launch': %w", err)
	}

	log.Debug("Launched fly application")

	args = []string{}

	// Add the MCP server to the MCP client configurations
	for client := range McpClients {
		if flag.GetBool(ctx, client) {
			log.Debugf("Adding %s to MCP client configuration", client)
			args = append(args, "--"+client)
		}
	}

	for _, config := range flag.GetStringArray(ctx, "config") {
		if config != "" {
			log.Debugf("Adding %s to MCP client configuration", config)
			args = append(args, "--config", config)
		}
	}

	tmpConfig := filepath.Join(tempDir, "mcpConfig.json")
	if flag.GetBool(ctx, "inspector") {
		// If the inspector flag is set, capture the MCP client configuration
		log.Debug("Adding inspector to MCP client configuration")
		args = append(args, "--config", tmpConfig)
	}

	if len(args) == 0 {
		log.Debug("No MCP client configuration flags provided")
	} else {
		args = append([]string{"mcp", "add"}, args...)
		args = append(args, "--name", name)

		if app := flag.GetString(ctx, "app"); app != "" {
			args = append(args, "--app", app)
		}
		if user := flag.GetString(ctx, "user"); user != "" {
			args = append(args, "--user", user)
		}
		if password := flag.GetString(ctx, "password"); password != "" {
			args = append(args, "--password", password)
		}
		if bearer := flag.GetBool(ctx, "bearer-token"); bearer {
			args = append(args, "--bearer-token")
		}
		if flycast := flag.GetBool(ctx, "flycast"); flycast {
			args = append(args, "--flycast")
		}

		// Run 'fly mcp add ...'
		cmd = exec.Command(flyctl, args...)
		cmd.Env = os.Environ()
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to run 'fly mcp add': %w", err)
		}

		log.Debug(strings.Join(args, " "))
	}

	// Deploy to a single machine
	cmd = exec.Command(flyctl, "deploy", "--ha=false")
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to run 'fly launch': %w", err)
	}

	log.Debug("Successfully completed MCP server launch and configuration")

	// If the inspector flag is set, run the MCP inspector
	if flag.GetBool(ctx, "inspector") {
		// Read the JSON file
		data, err := os.ReadFile(tmpConfig)
		if err != nil {
			fmt.Printf("Error reading file: %v\n", err)
			os.Exit(1)
		}

		// Parse the JSON data
		var config MCPConfig
		if err := json.Unmarshal(data, &config); err != nil {
			fmt.Printf("Error parsing JSON: %v\n", err)
			os.Exit(1)
		}

		args := []string{"-y", "@modelcontextprotocol/inspector"}
		for _, server := range config.MCPServers {
			args = append(args, server.Command)
			args = append(args, server.Args...)
			break
		}

		inspectorCmd := exec.Command("npx", args...)
		inspectorCmd.Env = os.Environ()
		inspectorCmd.Stdout = os.Stdout
		inspectorCmd.Stderr = os.Stderr
		inspectorCmd.Stdin = os.Stdin
		if err := inspectorCmd.Run(); err != nil {
			return fmt.Errorf("failed to run MCP inspector: %w", err)
		}
		log.Debug("MCP inspector launched")
	}

	return nil
}
