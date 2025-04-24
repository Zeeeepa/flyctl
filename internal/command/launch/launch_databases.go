package launch

import (
	"context"
	"fmt"
	"time"

	"github.com/samber/lo"
	fly "github.com/superfly/fly-go"
	"github.com/superfly/flyctl/gql"
	extensions_core "github.com/superfly/flyctl/internal/command/extensions/core"
	"github.com/superfly/flyctl/internal/command/launch/plan"
	"github.com/superfly/flyctl/internal/command/redis"
	"github.com/superfly/flyctl/internal/flyutil"
	"github.com/superfly/flyctl/internal/uiex"
	"github.com/superfly/flyctl/internal/uiexutil"
	"github.com/superfly/flyctl/iostreams"
)

// createDatabases creates databases requested by the plan
func (state *launchState) createDatabases(ctx context.Context) error {
	planStep := plan.GetPlanStep(ctx)

	if state.Plan.Postgres.FlyPostgres != nil && (planStep == "" || planStep == "postgres") {
		err := state.createFlyPostgres(ctx)
		if err != nil {
			// TODO(Ali): Make error printing here better.
			fmt.Fprintf(iostreams.FromContext(ctx).ErrOut, "Error creating Postgres cluster: %s\n", err)
		}
	}

	if state.Plan.Postgres.SupabasePostgres != nil && (planStep == "" || planStep == "postgres") {
		fmt.Fprintf(iostreams.FromContext(ctx).ErrOut, "Supabase Postgres is no longer supported.\n")
	}

	if state.Plan.Redis.UpstashRedis != nil && (planStep == "" || planStep == "redis") {
		err := state.createUpstashRedis(ctx)
		if err != nil {
			// TODO(Ali): Make error printing here better.
			fmt.Fprintf(iostreams.FromContext(ctx).ErrOut, "Error provisioning Upstash Redis: %s\n", err)
		}
	}

	if state.Plan.ObjectStorage.TigrisObjectStorage != nil && (planStep == "" || planStep == "tigris") {
		err := state.createTigrisObjectStorage(ctx)
		if err != nil {
			// TODO(Ali): Make error printing here better.
			fmt.Fprintf(iostreams.FromContext(ctx).ErrOut, "Error creating Tigris object storage: %s\n", err)
		}
	}

	// Run any initialization commands required for Postgres if it was installed
	if state.Plan.Postgres.Provider() != nil && state.sourceInfo != nil && (planStep == "" || planStep == "postgres") {
		for _, cmd := range state.sourceInfo.PostgresInitCommands {
			if cmd.Condition {
				if err := execInitCommand(ctx, cmd); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (state *launchState) createFlyPostgres(ctx context.Context) error {
	var (
		io         = iostreams.FromContext(ctx)
		pgPlan     = state.Plan.Postgres.FlyPostgres
		uiexClient = uiexutil.ClientFromContext(ctx)
	)

	// Get org and region
	org, err := state.Org(ctx)
	if err != nil {
		return err
	}
	region, err := state.Region(ctx)
	if err != nil {
		return err
	}

	// Create new managed Postgres cluster
	input := uiex.CreateClusterInput{
		Name:    pgPlan.AppName,
		Region:  region.Code,
		Plan:    "basic", // Default plan for now
		OrgSlug: org.Slug,
	}

	response, err := uiexClient.CreateCluster(ctx, input)
	if err != nil {
		return fmt.Errorf("failed creating managed postgres cluster: %w", err)
	}

	if response.Data.Status == nil {
		return fmt.Errorf("invalid cluster response: status is nil")
	}

	// Wait for cluster to be ready
	fmt.Fprintf(io.Out, "Waiting for cluster to be ready...\n")
	for {
		cluster, err := uiexClient.GetManagedClusterById(ctx, response.Data.Id)
		if err != nil {
			return fmt.Errorf("failed checking cluster status: %w", err)
		}

		if cluster.Data.Status == "ready" {
			break
		}

		if cluster.Data.Status == "error" {
			return fmt.Errorf("cluster creation failed")
		}

		time.Sleep(5 * time.Second)
	}

	// Create a user for the app
	userInput := uiex.CreateUserInput{
		DbName:   "postgres",
		UserName: state.Plan.AppName,
	}

	userResponse, err := uiexClient.CreateUser(ctx, response.Data.Id, userInput)
	if err != nil {
		return fmt.Errorf("failed creating database user: %w", err)
	}

	// Set the connection string as a secret
	secrets := map[string]string{
		"DATABASE_URL": userResponse.ConnectionUri,
	}

	_, err = flyutil.ClientFromContext(ctx).SetSecrets(ctx, state.Plan.AppName, secrets)
	if err != nil {
		return fmt.Errorf("failed setting database connection string: %w", err)
	}

	fmt.Fprintf(io.Out, "Managed Postgres cluster %s created successfully!\n", pgPlan.AppName)
	fmt.Fprintf(io.Out, "  Organization: %s\n", org.Slug)
	fmt.Fprintf(io.Out, "  Region: %s\n", region.Code)
	fmt.Fprintf(io.Out, "  Plan: %s\n", response.Data.Plan)
	fmt.Fprintf(io.Out, "  Status: %s\n", *response.Data.Status)
	fmt.Fprintf(io.Out, "  Connection string saved as DATABASE_URL\n")

	return nil
}

func (state *launchState) createUpstashRedis(ctx context.Context) error {
	redisPlan := state.Plan.Redis.UpstashRedis
	dbName := fmt.Sprintf("%s-redis", state.Plan.AppName)
	org, err := state.Org(ctx)
	if err != nil {
		return err
	}
	region, err := state.Region(ctx)
	if err != nil {
		return err
	}

	var readReplicaRegions []fly.Region
	{
		client := flyutil.ClientFromContext(ctx)
		regions, _, err := client.PlatformRegions(ctx)
		if err != nil {
			return err
		}
		for _, code := range redisPlan.ReadReplicas {
			if region, ok := lo.Find(regions, func(r fly.Region) bool { return r.Code == code }); ok {
				readReplicaRegions = append(readReplicaRegions, region)
			} else {
				return fmt.Errorf("region %s not found", code)
			}
		}
	}

	db, err := redis.Create(ctx, org, dbName, &region, len(readReplicaRegions) == 0, redisPlan.Eviction, &readReplicaRegions)
	if err != nil {
		return err
	}
	return redis.AttachDatabase(ctx, db, state.Plan.AppName)
}

func (state *launchState) createTigrisObjectStorage(ctx context.Context) error {
	tigrisPlan := state.Plan.ObjectStorage.TigrisObjectStorage

	org, err := state.Org(ctx)
	if err != nil {
		return err
	}

	params := extensions_core.ExtensionParams{
		Provider:       "tigris",
		Organization:   org,
		AppName:        state.Plan.AppName,
		OverrideName:   fly.Pointer(tigrisPlan.Name),
		OverrideRegion: state.Plan.RegionCode,
		Options: gql.AddOnOptions{
			"public":     tigrisPlan.Public,
			"accelerate": tigrisPlan.Accelerate,
			"website": map[string]interface{}{
				"domain_name": tigrisPlan.WebsiteDomainName,
			},
		},
		OverrideExtensionSecretKeyNames: state.sourceInfo.OverrideExtensionSecretKeyNames,
	}

	_, err = extensions_core.ProvisionExtension(ctx, params)

	if err != nil {
		return err
	}

	return err
}
