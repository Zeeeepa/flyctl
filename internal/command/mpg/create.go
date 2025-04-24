package mpg

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/superfly/fly-go"
	"github.com/superfly/flyctl/internal/command"
	"github.com/superfly/flyctl/internal/flag"
	"github.com/superfly/flyctl/internal/prompt"
	"github.com/superfly/flyctl/internal/uiex"
	"github.com/superfly/flyctl/internal/uiexutil"
	"github.com/superfly/flyctl/iostreams"
)

// Allowed MPG regions
var allowedMPGRegions = []string{"ams", "fra", "iad", "ord", "syd", "lax"}

type CreateClusterParams struct {
	Name          string
	OrgSlug       string
	Region        string
	Plan          string
	Nodes         int
	VolumeSizeGB  int
	EnableBackups bool
	AutoStop      bool
}

func newCreate() *cobra.Command {
	const (
		short = "Create a new Managed Postgres cluster"
		long  = short + "\n"
	)

	cmd := command.New("create", short, long, runCreate,
		command.RequireSession,
		command.RequireUiex,
	)

	flag.Add(
		cmd,
		flag.Region(),
		flag.Org(),
		flag.String{
			Name:        "name",
			Shorthand:   "n",
			Description: "The name of your Postgres cluster",
		},
		flag.String{
			Name:        "plan",
			Description: "The plan to use for the Postgres cluster (development, production, etc)",
		},
		flag.Int{
			Name:        "nodes",
			Description: "Number of nodes in the cluster",
			Default:     1,
		},
		flag.Int{
			Name:        "volume-size",
			Description: "The volume size in GB",
			Default:     10,
		},
		flag.Bool{
			Name:        "enable-backups",
			Description: "Enable WAL-based backups",
			Default:     true,
		},
		flag.Bool{
			Name:        "auto-stop",
			Description: "Automatically stop the cluster when not in use",
			Default:     false,
		},
	)

	return cmd
}

func runCreate(ctx context.Context) error {
	var (
		io      = iostreams.FromContext(ctx)
		appName = flag.GetString(ctx, "name")
		err     error
	)

	if appName == "" {
		appName, err = prompt.SelectAppName(ctx)
		if err != nil {
			return err
		}
	}

	org, err := prompt.Org(ctx)
	if err != nil {
		return err
	}

	// Get all regions and filter for MPG allowed regions
	regionsFuture := prompt.PlatformRegions(ctx)
	regions, err := regionsFuture.Get()
	if err != nil {
		return err
	}

	var mpgRegions []fly.Region
	for _, region := range regions.Regions {
		for _, allowed := range allowedMPGRegions {
			if region.Code == allowed {
				mpgRegions = append(mpgRegions, region)
				break
			}
		}
	}

	if len(mpgRegions) == 0 {
		return fmt.Errorf("no valid regions found for Managed Postgres")
	}

	// Check if region was specified via flag
	regionCode := flag.GetString(ctx, "region")
	var selectedRegion *fly.Region

	if regionCode != "" {
		// Find the specified region in the allowed regions
		for _, region := range mpgRegions {
			if region.Code == regionCode {
				selectedRegion = &region
				break
			}
		}
		if selectedRegion == nil {
			return fmt.Errorf("region %s is not available for Managed Postgres. Available regions: %v", regionCode, allowedMPGRegions)
		}
	} else {
		// Create region options for prompt
		var regionOptions []string
		for _, region := range mpgRegions {
			regionOptions = append(regionOptions, fmt.Sprintf("%s (%s)", region.Name, region.Code))
		}

		var selectedIndex int
		if err := prompt.Select(ctx, &selectedIndex, "Select a region for your Managed Postgres cluster", "", regionOptions...); err != nil {
			return err
		}

		selectedRegion = &mpgRegions[selectedIndex]
	}

	plan := flag.GetString(ctx, "plan")
	if plan == "" {
		plan = "basic" // Default plan
	}

	params := &CreateClusterParams{
		Name:          appName,
		OrgSlug:       org.Slug,
		Region:        selectedRegion.Code,
		Plan:          plan,
		Nodes:         flag.GetInt(ctx, "nodes"),
		VolumeSizeGB:  flag.GetInt(ctx, "volume-size"),
		EnableBackups: flag.GetBool(ctx, "enable-backups"),
		AutoStop:      flag.GetBool(ctx, "auto-stop"),
	}

	uiexClient := uiexutil.ClientFromContext(ctx)

	input := uiex.CreateClusterInput{
		Name:    params.Name,
		Region:  params.Region,
		Plan:    params.Plan,
		OrgSlug: params.OrgSlug,
	}

	response, err := uiexClient.CreateCluster(ctx, input)
	if err != nil {
		return fmt.Errorf("failed creating managed postgres cluster: %w", err)
	}

	// Wait for cluster to be ready
	fmt.Fprintf(io.Out, "Waiting for cluster to be ready...\n")
	for {
		cluster, err := uiexClient.GetManagedClusterById(ctx, response.Data.Id)
		if err != nil {
			return fmt.Errorf("failed checking cluster status: %w", err)
		}

		if cluster.Data.Id == "" {
			return fmt.Errorf("invalid cluster response: no cluster ID")
		}

		if cluster.Data.Status == "ready" {
			break
		}

		if cluster.Data.Status == "error" {
			return fmt.Errorf("cluster creation failed")
		}

		time.Sleep(5 * time.Second)
	}

	fmt.Fprintf(io.Out, "Managed Postgres cluster %s created successfully!\n", params.Name)
	fmt.Fprintf(io.Out, "  Organization: %s\n", params.OrgSlug)
	fmt.Fprintf(io.Out, "  Region: %s\n", params.Region)
	fmt.Fprintf(io.Out, "  Plan: %s\n", params.Plan)
	fmt.Fprintf(io.Out, "  Replicas: %d\n", response.Data.Replicas)
	fmt.Fprintf(io.Out, "  Disk: %dGB\n", response.Data.Disk)

	return nil
}
