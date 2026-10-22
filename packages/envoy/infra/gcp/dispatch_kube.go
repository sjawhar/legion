package gcp

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	kubernetes "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes"
	apiextensions "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/apiextensions"
	appsv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/apps/v1"
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	networkingv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/networking/v1"
	rbacv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/rbac/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
)

type dispatchKubernetes struct {
	kubeconfig pulumi.StringOutput
}

// dispatchSOCKS5Port is the port the front door's tailscaled binds its userspace SOCKS5 proxy on,
// which its Service publishes, its NetworkPolicy admits, and the two workloads that dial the NATS
// hub through it name.
const dispatchSOCKS5Port = 1055

// clusterLocal is the in-cluster DNS name of Service name in namespace: what one pod calls
// another workload's Service at.
func clusterLocal(name, namespace string) string {
	return name + "." + namespace + ".svc.cluster.local"
}

// deploymentStrategy maps a resolved strategy onto a Deployment's. A Recreate strategy carries no
// rolling bounds, and so registers no rollingUpdate key at all.
func deploymentStrategy(strategy dispatchStrategy) *appsv1.DeploymentStrategyArgs {
	args := &appsv1.DeploymentStrategyArgs{Type: pulumi.String(strategy.strategyType)}
	if rolling := strategy.rollingUpdate; rolling != nil {
		args.RollingUpdate = &appsv1.RollingUpdateDeploymentArgs{
			MaxUnavailable: pulumi.Int(rolling.maxUnavailable),
			MaxSurge:       pulumi.Int(rolling.maxSurge),
		}
	}
	return args
}

func renderKubeconfig(endpoint, ca, name string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: %s
  cluster:
    server: https://%s
    certificate-authority-data: %s
contexts:
- name: %s
  context:
    cluster: %s
    user: %s
current-context: %s
users:
- name: %s
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: gke-gcloud-auth-plugin
      installHint: Install gke-gcloud-auth-plugin (gcloud components install gke-gcloud-auth-plugin)
      provideClusterInfo: true
      interactiveMode: Never
`, name, endpoint, ca, name, name, name, name, name)
}

// outputString is one resolved value of pulumi.All as the string it holds. A GCP output field is
// a *string, which the provider can leave nil (the cluster's WorkloadPool among them), so a nil
// or another type is an error rather than a panic inside ApplyT, which would kill the provider
// process.
func outputString(value any) (string, error) {
	switch value := value.(type) {
	case string:
		return value, nil
	case *string:
		if value == nil {
			return "", errors.New("resolved to a nil *string")
		}
		return *value, nil
	}
	return "", fmt.Errorf("resolved to a %T, not a string", value)
}

func resources(cpu, memory string) *corev1.ResourceRequirementsArgs {
	values := pulumi.StringMap{"cpu": pulumi.String(cpu), "memory": pulumi.String(memory)}
	return &corev1.ResourceRequirementsArgs{Requests: values, Limits: values}
}

func valueEnv(name, value string) corev1.EnvVarArgs {
	return corev1.EnvVarArgs{Name: pulumi.String(name), Value: pulumi.String(value)}
}

func secretEnv(name, secret, key string) corev1.EnvVarArgs {
	return corev1.EnvVarArgs{
		Name: pulumi.String(name),
		ValueFrom: &corev1.EnvVarSourceArgs{SecretKeyRef: &corev1.SecretKeySelectorArgs{
			Name: pulumi.String(secret), Key: pulumi.String(key),
		}},
	}
}

// refuseDispatchSeed is value, a plain-typed Input of the component (the NATS seed secret's id),
// with a seed refused once it resolves: the output fails naming input and never the value, so no
// child is registered with it.
func refuseDispatchSeed(value pulumi.StringInput, dispatch, input string) pulumi.StringOutput {
	return value.ToStringOutput().ApplyT(func(v string) (string, error) {
		if bus.HoldsSeed(v) {
			return "", fmt.Errorf("%s: %w", dispatch, core.SeedRefusal(input))
		}
		return v, nil
	}).(pulumi.StringOutput)
}

// refuseDispatchSecret is the one check every secret credential input of the component runs once
// it resolves (tailnetAuthKey, signInClientSecret): a blank or whitespace-only value is a
// credential that cannot work (the front door cannot join the tailnet, Dispatch refuses a blank
// sign-in secret at boot), so it is refused here, and so is one holding an nkey seed. Each
// refusal names input and never the value.
func refuseDispatchSecret(v, dispatch, input string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("%s: %w", dispatch, core.InputErrorf(input, "%s is blank, which no credential is", input))
	}
	if bus.HoldsSeed(v) {
		return fmt.Errorf("%s: %w", dispatch, core.SeedRefusal(input))
	}
	return nil
}

// refuseDispatchSeedInSecret is value, a secret Input of the component, as a secret, with a blank
// value or one holding an nkey seed refused once it resolves: the output fails naming input and
// never the value, so no child is registered with it. ApplyT keeps value's secretness, and
// ToSecret marks the result secret whatever value's own marking.
func refuseDispatchSeedInSecret(value pulumi.StringInput, dispatch, input string) pulumi.StringOutput {
	return pulumi.ToSecret(value.ToStringOutput().ApplyT(func(v string) (string, error) {
		if err := refuseDispatchSecret(v, dispatch, input); err != nil {
			return "", err
		}
		return v, nil
	})).(pulumi.StringOutput)
}

// dispatchListenerEnv renders the envoy-listener container's environment from the one NATS URL
// once it resolves. core's Env names its own inputs — natsUrls by index and socks5Proxy, neither
// an input of gcp:Dispatch — so each refusal is mapped onto natsUrl, this component's input. The
// SOCKS5 proxy is fixed (the front door's Service, socks5Proxy), so its refusal means the URL
// names a local server, which the pod's NATS clients can never reach through the tailnet proxy.
// The listener serves on every address of its pod, and its API refuses to start on a non-loopback
// listen host without a bearer (packages/envoy/cmd/listener/apiauth.go), so the environment names
// the bearer in Secret listenerSecret.
func dispatchListenerEnv(hostname, socks5Proxy, listenerSecret, url string) (corev1.EnvVarArray, error) {
	env, err := (core.ListenerConfig{
		MachineID:   hostname,
		Port:        core.DefaultListenerPort,
		ListenHost:  "0.0.0.0",
		SOCKS5Proxy: socks5Proxy,
	}).Env([]string{url})
	if err != nil {
		var input *core.InputError
		if errors.As(err, &input) && input.Input == "socks5Proxy" {
			return nil, core.InputErrorf("natsUrl", "natsUrl names a local NATS server; the pods reach NATS only through the front door's SOCKS5 proxy, so the URL must be remote (the hub's tailnet name)")
		}
		return nil, core.InputErrorf("natsUrl", "%s", strings.ReplaceAll(err.Error(), "natsUrls[0]", "natsUrl"))
	}
	values := make(corev1.EnvVarArray, 0, len(env)+2)
	for _, item := range env {
		name, value, _ := strings.Cut(item, "=")
		values = append(values, valueEnv(name, value))
	}
	values = append(values, valueEnv("NATS_NKEY_SEED_FILE", "/var/run/secrets/envoy/nats-seed"))
	values = append(values, secretEnv("ENVOY_API_TOKEN", listenerSecret, "ENVOY_API_TOKEN"))
	return values, nil
}

func newDispatchKubernetes(ctx *pulumi.Context, comp *Dispatch, name string, args *DispatchArgs, s dispatchSettings, cloud *dispatchCloud) (*dispatchKubernetes, error) {
	dispatch := dispatchType + " " + name
	kubeconfig := pulumi.All(cloud.cluster.Endpoint, cloud.cluster.MasterAuth.ClusterCaCertificate(), cloud.cluster.Name).ApplyT(func(values []interface{}) (string, error) {
		parts := make([]string, len(values))
		for i, what := range []string{"the cluster's endpoint", "the cluster's CA certificate", "the cluster's name"} {
			value, err := outputString(values[i])
			if err != nil {
				return "", fmt.Errorf("%s: %s: %w", dispatch, what, err)
			}
			parts[i] = value
		}
		return renderKubeconfig(parts[0], parts[1], parts[2]), nil
	}).(pulumi.StringOutput)
	provider, err := kubernetes.NewProvider(ctx, s.hostname+"-kubernetes", &kubernetes.ProviderArgs{
		Kubeconfig: kubeconfig, EnableServerSideApply: pulumi.Bool(true),
	}, pulumi.Parent(comp))
	if err != nil {
		return nil, err
	}
	child := []pulumi.ResourceOption{pulumi.Parent(comp), pulumi.Provider(provider), pulumi.DependsOn([]pulumi.Resource{cloud.cluster})}
	metadata := func(name string) *metav1.ObjectMetaArgs {
		return &metav1.ObjectMetaArgs{Name: pulumi.String(name), Namespace: pulumi.String(s.namespace)}
	}
	if !s.existingNamespace {
		if _, err := corev1.NewNamespace(ctx, s.namespace, &corev1.NamespaceArgs{Metadata: &metav1.ObjectMetaArgs{Name: pulumi.String(s.namespace)}}, child...); err != nil {
			return nil, err
		}
	}
	if _, err := corev1.NewServiceAccount(ctx, s.hostname, &corev1.ServiceAccountArgs{Metadata: metadata(s.hostname)}, child...); err != nil {
		return nil, err
	}
	stateName := s.hostname + "-tailscale-state"
	_, err = rbacv1.NewRole(ctx, stateName, &rbacv1.RoleArgs{
		Metadata: metadata(stateName),
		Rules: rbacv1.PolicyRuleArray{
			rbacv1.PolicyRuleArgs{ApiGroups: pulumi.StringArray{pulumi.String("")}, Resources: pulumi.StringArray{pulumi.String("secrets")}, Verbs: pulumi.StringArray{pulumi.String("create")}},
			rbacv1.PolicyRuleArgs{ApiGroups: pulumi.StringArray{pulumi.String("")}, Resources: pulumi.StringArray{pulumi.String("secrets")}, ResourceNames: pulumi.StringArray{pulumi.String(stateName)}, Verbs: pulumi.StringArray{pulumi.String("get"), pulumi.String("update"), pulumi.String("patch")}},
		},
	}, child...)
	if err != nil {
		return nil, err
	}
	if _, err := rbacv1.NewRoleBinding(ctx, stateName, &rbacv1.RoleBindingArgs{
		Metadata: metadata(stateName),
		RoleRef:  rbacv1.RoleRefArgs{ApiGroup: pulumi.String("rbac.authorization.k8s.io"), Kind: pulumi.String("Role"), Name: pulumi.String(stateName)},
		Subjects: rbacv1.SubjectArray{rbacv1.SubjectArgs{Kind: pulumi.String("ServiceAccount"), Name: pulumi.String(s.hostname), Namespace: pulumi.String(s.namespace)}},
	}, child...); err != nil {
		return nil, err
	}
	authKeyName := s.hostname + "-tailnet-auth-key"
	if _, err := corev1.NewSecret(ctx, authKeyName, &corev1.SecretArgs{Metadata: metadata(authKeyName), StringData: pulumi.StringMap{"TS_AUTHKEY": refuseDispatchSeedInSecret(args.TailnetAuthKey, dispatch, "tailnetAuthKey")}}, child...); err != nil {
		return nil, err
	}
	dispatchSecretName := s.hostname + "-dispatch"
	databaseURL := pulumi.Sprintf("postgres://dispatch:%s@127.0.0.1:5432/dispatch?sslmode=disable", cloud.password.Result)
	if _, err := corev1.NewSecret(ctx, dispatchSecretName, &corev1.SecretArgs{Metadata: metadata(dispatchSecretName), StringData: pulumi.StringMap{
		"DATABASE_URL": databaseURL, "DISPATCH_AGENT_TOKEN": cloud.agentToken.Result, "DISPATCH_SIGNING_KEY": cloud.signing.Result, "DISPATCH_SIGNIN_CLIENT_SECRET": refuseDispatchSeedInSecret(args.SignInClientSecret, dispatch, "signInClientSecret"),
	}}, child...); err != nil {
		return nil, err
	}
	// The listener's Deployment, Service and bearer share one name. The bearer is a Secret of its
	// own: the listener's pod names this one and never the component's dispatch Secret, which
	// holds DATABASE_URL and DISPATCH_SIGNING_KEY.
	listenerName := s.hostname + "-listener"
	if _, err := corev1.NewSecret(ctx, listenerName, &corev1.SecretArgs{
		Metadata: metadata(listenerName), StringData: pulumi.StringMap{"ENVOY_API_TOKEN": cloud.listenerToken.Result},
	}, child...); err != nil {
		return nil, err
	}
	serveName := s.hostname + "-serve"
	if _, err := corev1.NewConfigMap(ctx, serveName, &corev1.ConfigMapArgs{Metadata: metadata(serveName), Data: pulumi.StringMap{"serve.json": pulumi.String(s.serveConfig)}}, child...); err != nil {
		return nil, err
	}
	seedName := s.hostname + "-nats-seed"
	seedPath := refuseDispatchSeed(args.NatsSeedSecret, dispatch, "natsSeedSecret").ApplyT(func(secret string) string {
		return strings.TrimSuffix(secret, "/") + "/versions/latest"
	}).(pulumi.StringOutput)
	if _, err := apiextensions.NewCustomResource(ctx, seedName, &apiextensions.CustomResourceArgs{
		ApiVersion: pulumi.String("secrets-store.csi.x-k8s.io/v1"), Kind: pulumi.String("SecretProviderClass"), Metadata: metadata(seedName),
		OtherFields: kubernetes.UntypedArgs{"spec": pulumi.Map{"provider": pulumi.String("gke"), "parameters": pulumi.Map{"secrets": seedPath.ApplyT(func(path string) string { return "- resourceName: " + path + "\n  path: nats-seed\n" }).(pulumi.StringOutput)}}},
	}, child...); err != nil {
		return nil, err
	}

	tokenClassName := s.hostname + "-agent-secrets-token"
	if s.agentSecretsURL != "" {
		tokenPath := refuseDispatchSeed(args.AgentSecretsTokenSecret, dispatch, "agentSecretsTokenSecret").ApplyT(func(secret string) string {
			return strings.TrimSuffix(secret, "/") + "/versions/latest"
		}).(pulumi.StringOutput)
		if _, err := apiextensions.NewCustomResource(ctx, tokenClassName, &apiextensions.CustomResourceArgs{
			ApiVersion: pulumi.String("secrets-store.csi.x-k8s.io/v1"), Kind: pulumi.String("SecretProviderClass"), Metadata: metadata(tokenClassName),
			OtherFields: kubernetes.UntypedArgs{"spec": pulumi.Map{"provider": pulumi.String("gke"), "parameters": pulumi.Map{"secrets": tokenPath.ApplyT(func(path string) string { return "- resourceName: " + path + "\n  path: agent-secrets-token\n" }).(pulumi.StringOutput)}}},
		}, child...); err != nil {
			return nil, err
		}
	}

	// Every pod reaches the front door's SOCKS5 proxy and the listener over the network now that
	// each runs in a pod of its own: one tailnet node holds the hostname and the single-use auth
	// key's state, and one listener binds the machine id's exclusive durable, while the app scales.
	tailnetName := s.hostname + "-tailnet"
	socks5Port := strconv.Itoa(dispatchSOCKS5Port)
	listenerPort := strconv.Itoa(core.DefaultListenerPort)
	socks5Proxy := clusterLocal(tailnetName, s.namespace) + ":" + socks5Port
	listenerURL := "http://" + clusterLocal(listenerName, s.namespace) + ":" + listenerPort

	// natsURL is the one NATS URL once checked: every container reads this output rather than the
	// raw input, so envoy-dispatch's NATS_URLS is never an unchecked copy beside the listener's
	// checked environment.
	natsURL := args.NatsUrl.ToStringOutput().ApplyT(func(url string) (string, error) {
		if _, err := dispatchListenerEnv(s.hostname, socks5Proxy, listenerName, url); err != nil {
			return "", fmt.Errorf("%s: %w", dispatch, err)
		}
		return url, nil
	}).(pulumi.StringOutput)
	listenerEnv := natsURL.ApplyT(func(url string) (corev1.EnvVarArray, error) {
		env, err := dispatchListenerEnv(s.hostname, socks5Proxy, listenerName, url)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", dispatch, err)
		}
		return env, nil
	}).(corev1.EnvVarArrayOutput)

	// deployment registers one of the component's three workloads. A pod wears app=<its own name>
	// alone, which its Deployment's selector matches, so Service <hostname>, which selects
	// app=<hostname>, sends user traffic to the app's pods and to no other workload's.
	deployment := func(name string, replicas int, strategy *appsv1.DeploymentStrategyArgs, pod corev1.PodSpecArgs, opts ...pulumi.ResourceOption) (*appsv1.Deployment, error) {
		return appsv1.NewDeployment(ctx, name, &appsv1.DeploymentArgs{
			Metadata: metadata(name),
			Spec: appsv1.DeploymentSpecArgs{
				Replicas: pulumi.Int(replicas),
				Strategy: strategy,
				Selector: &metav1.LabelSelectorArgs{MatchLabels: pulumi.StringMap{"app": pulumi.String(name)}},
				Template: corev1.PodTemplateSpecArgs{
					Metadata: &metav1.ObjectMetaArgs{Labels: pulumi.StringMap{"app": pulumi.String(name)}},
					Spec:     pod,
				},
			},
		}, append(opts, child...)...)
	}
	service := func(name string, port int) (*corev1.Service, error) {
		return corev1.NewService(ctx, name, &corev1.ServiceArgs{
			Metadata: metadata(name),
			Spec: &corev1.ServiceSpecArgs{
				Selector: pulumi.StringMap{"app": pulumi.String(name)},
				Ports:    corev1.ServicePortArray{corev1.ServicePortArgs{Port: pulumi.Int(port), TargetPort: pulumi.Int(port)}},
			},
		}, child...)
	}
	// Both single-replica workloads are recreated, never rolled, whatever the app's replica count:
	// a second tailnet node would contend for the one state Secret the Role names, and a second
	// listener with the same machine id never binds its exclusive durable.
	recreate := dispatchStrategyFor(minDispatchReplicas)

	seedMount := corev1.VolumeMountArray{corev1.VolumeMountArgs{Name: pulumi.String("nats-seed"), MountPath: pulumi.String("/var/run/secrets/envoy"), ReadOnly: pulumi.Bool(true)}}
	seedVolume := corev1.VolumeArray{corev1.VolumeArgs{Name: pulumi.String("nats-seed"), Csi: &corev1.CSIVolumeSourceArgs{Driver: pulumi.String("secrets-store-gke.csi.k8s.io"), ReadOnly: pulumi.Bool(true), VolumeAttributes: pulumi.StringMap{"secretProviderClass": pulumi.String(seedName)}}}}
	tailnetHealth := func() *corev1.ProbeArgs {
		return &corev1.ProbeArgs{Exec: &corev1.ExecActionArgs{Command: pulumi.StringArray{pulumi.String("wget"), pulumi.String("-qO-"), pulumi.String("http://127.0.0.1:9002/healthz")}}}
	}

	// The front door: the tailnet node every reader reaches Dispatch at, and the pod's SOCKS5
	// proxy, which is how the app and the listener reach the NATS hub at its tailnet name. The
	// proxy binds every address, since its callers are pods of their own now, not this pod's
	// loopback.
	tailnetStartup := tailnetHealth()
	tailnetStartup.PeriodSeconds, tailnetStartup.FailureThreshold = pulumi.Int(5), pulumi.Int(36)
	tailnetReady := tailnetHealth()
	tailnetReady.PeriodSeconds = pulumi.Int(10)
	tailnet, err := deployment(tailnetName, 1, deploymentStrategy(recreate), corev1.PodSpecArgs{
		ServiceAccountName: pulumi.String(s.hostname), EnableServiceLinks: pulumi.Bool(false),
		Containers: corev1.ContainerArray{
			corev1.ContainerArgs{Name: pulumi.String("tailscale"), Image: pulumi.String(s.images.tailscale), Resources: resources("100m", "128Mi"), Env: corev1.EnvVarArray{
				secretEnv("TS_AUTHKEY", authKeyName, "TS_AUTHKEY"),
				valueEnv("TS_AUTH_ONCE", "true"), valueEnv("TS_KUBE_SECRET", stateName), valueEnv("TS_USERSPACE", "true"), valueEnv("TS_SOCKS5_SERVER", "0.0.0.0:"+socks5Port), valueEnv("TS_SERVE_CONFIG", "/etc/tailscale/serve.json"), valueEnv("TS_HOSTNAME", s.hostname), valueEnv("TS_EXTRA_ARGS", "--advertise-tags="+strings.Join(s.tags, ",")), valueEnv("TS_ENABLE_HEALTH_CHECK", "true"), valueEnv("TS_LOCAL_ADDR_PORT", "127.0.0.1:9002"),
			}, VolumeMounts: corev1.VolumeMountArray{corev1.VolumeMountArgs{Name: pulumi.String("serve"), MountPath: pulumi.String("/etc/tailscale")}}, StartupProbe: tailnetStartup, ReadinessProbe: tailnetReady},
		},
		Volumes: corev1.VolumeArray{corev1.VolumeArgs{Name: pulumi.String("serve"), ConfigMap: &corev1.ConfigMapVolumeSourceArgs{Name: pulumi.String(serveName)}}},
	})
	if err != nil {
		return nil, err
	}
	tailnetService, err := service(tailnetName, dispatchSOCKS5Port)
	if err != nil {
		return nil, err
	}

	// Both other workloads dial the NATS hub through the front door's proxy, so each waits for its
	// pod and its Service: envoy-dispatch exits 1 when it cannot reach NATS and answers 503 on
	// /healthz until it connects, and envoy-listener exits on a failed initial connect
	// (packages/envoy/cmd/listener/main.go), so a pod started before the proxy exists never goes
	// ready and the rollout waits it out.
	frontDoor := pulumi.DependsOn([]pulumi.Resource{tailnet, tailnetService})
	// Both also mount the NATS seed, which the pod can read only once its principal holds
	// roles/secretmanager.secretAccessor on the secret, so each waits for that grant too rather
	// than racing IAM on the first deployment.
	seedGrant := pulumi.DependsOn([]pulumi.Resource{cloud.seedAccessor})

	// The listener runs under the app's ServiceAccount, which is what mounts the NATS seed through
	// Workload Identity.
	if _, err := deployment(listenerName, 1, deploymentStrategy(recreate), corev1.PodSpecArgs{
		ServiceAccountName: pulumi.String(s.hostname), EnableServiceLinks: pulumi.Bool(false),
		Containers: corev1.ContainerArray{
			corev1.ContainerArgs{Name: pulumi.String("envoy-listener"), Image: pulumi.String(s.images.envoy), Resources: resources("200m", "512Mi"), Env: listenerEnv, VolumeMounts: seedMount, ReadinessProbe: &corev1.ProbeArgs{Exec: &corev1.ExecActionArgs{Command: pulumi.StringArray{pulumi.String("curl"), pulumi.String("-fsS"), pulumi.String("http://127.0.0.1:" + listenerPort + "/healthz")}}}},
		},
		Volumes: seedVolume,
	}, frontDoor, seedGrant); err != nil {
		return nil, err
	}
	if _, err := service(listenerName, core.DefaultListenerPort); err != nil {
		return nil, err
	}

	// A pod leaves routing asynchronously, and it does so alongside its own shutdown rather than
	// after it: the control plane takes the pod out of its EndpointSlice as soon as the deletion
	// timestamp appears, concurrently with the kubelet beginning graceful termination - the
	// pre-stop hook first, SIGTERM once the hook returns - and that removal then has to reach
	// every node's proxy and Tailscale Serve. envoy-dispatch closes its HTTP listener as the
	// signal arrives (packages/envoy/cmd/dispatch/shutdown.go), so the pre-stop wait runs
	// alongside the propagation and is sized to outlast it, holding the container open across that
	// window: a pod still being sent requests still answers them, and the image
	// (debian:bookworm-slim) carries /bin/sleep. The kubelet spends the hook and the signalled
	// shutdown inside the one terminationGracePeriodSeconds, so a wait is taken out of the
	// server's shutdown unless the pod's budget covers both: the 35 s below is these 5 s of
	// routing window plus the 30 s cmd/dispatch/shutdown.go builds its shutdown for
	// (stopGrace = 30 * time.Second). The two values are coupled and neither may be raised alone; a
	// wait the grace period does not cover spends the document service's five-second reserve and
	// kills a write still waiting on a lock mid-commit.
	// Only the app's container waits: the front door and the listener are single-replica Recreate
	// workloads with no second pod to route to, so a wait there would only lengthen their outage,
	// and neither sets a grace period.
	appPreStop := &corev1.LifecycleArgs{PreStop: &corev1.LifecycleHandlerArgs{
		Exec: &corev1.ExecActionArgs{Command: pulumi.StringArray{pulumi.String("sleep"), pulumi.String("5")}},
	}}
	// The app's container alone carries the broker relay and the GitHub App: the listener and the
	// front door take neither.
	appEnv := corev1.EnvVarArray{
		valueEnv("HOME", "/home/envoy"), secretEnv("DATABASE_URL", dispatchSecretName, "DATABASE_URL"), secretEnv("DISPATCH_AGENT_TOKEN", dispatchSecretName, "DISPATCH_AGENT_TOKEN"), secretEnv("DISPATCH_SIGNING_KEY", dispatchSecretName, "DISPATCH_SIGNING_KEY"), secretEnv("DISPATCH_SIGNIN_CLIENT_SECRET", dispatchSecretName, "DISPATCH_SIGNIN_CLIENT_SECRET"),
		valueEnv("DISPATCH_SIGNIN_ISSUER", "https://accounts.google.com"), valueEnv("DISPATCH_SIGNIN_CLIENT_ID", s.signIn.clientID), valueEnv("DISPATCH_SIGNIN_DOMAIN", s.signIn.domain), valueEnv("DISPATCH_SERVER_URL", "https://"+s.hostname+"."+s.tailnetDNSName), valueEnv("DISPATCH_LISTEN_HOST", "0.0.0.0"), corev1.EnvVarArgs{Name: pulumi.String("NATS_URLS"), Value: natsURL}, valueEnv("ENVOY_ALLOW_REMOTE_NATS", "1"), valueEnv("NATS_NKEY_SEED_FILE", "/var/run/secrets/envoy/nats-seed"), valueEnv("ENVOY_NATS_SOCKS5_PROXY", socks5Proxy), valueEnv("ENVOY_URL", listenerURL), secretEnv("ENVOY_TOKEN", listenerName, "ENVOY_API_TOKEN"),
	}
	appMounts := append(corev1.VolumeMountArray{}, seedMount...)
	appVolumes := append(corev1.VolumeArray{}, seedVolume...)
	appGrants := seedGrant
	if s.agentSecretsURL != "" {
		// The token is read at run time from Secret Manager through a SecretProviderClass of its
		// own, mounted as a read-only file of a volume of its own into envoy-dispatch alone;
		// DISPATCH_AGENT_SECRETS_TOKEN_FILE names that file. The app's pods mount it, so they wait
		// for the grant that lets them, as they do for the seed's.
		appEnv = append(appEnv, valueEnv("DISPATCH_AGENT_SECRETS_URL", s.agentSecretsURL), valueEnv("DISPATCH_AGENT_SECRETS_TOKEN_FILE", "/var/run/secrets/dispatch/agent-secrets-token"))
		appMounts = append(appMounts, corev1.VolumeMountArgs{Name: pulumi.String("agent-secrets-token"), MountPath: pulumi.String("/var/run/secrets/dispatch"), ReadOnly: pulumi.Bool(true)})
		appVolumes = append(appVolumes, corev1.VolumeArgs{Name: pulumi.String("agent-secrets-token"), Csi: &corev1.CSIVolumeSourceArgs{Driver: pulumi.String("secrets-store-gke.csi.k8s.io"), ReadOnly: pulumi.Bool(true), VolumeAttributes: pulumi.StringMap{"secretProviderClass": pulumi.String(tokenClassName)}}})
		appGrants = pulumi.DependsOn([]pulumi.Resource{cloud.seedAccessor, cloud.tokenAccessor})
	}
	if s.appClientID != "" {
		// The App key is one key of a Kubernetes Secret this component never creates: it exists at
		// run time (GKE SecretSync), so the Deployment does not wait on it.
		appEnv = append(appEnv, valueEnv("DISPATCH_APP_CLIENT_ID", s.appClientID), valueEnv("DISPATCH_APP_ID", s.appID), secretEnv("DISPATCH_APP_PEM_B64", s.appKeySecretName, s.appKeySecretKey))
	}
	if _, err := deployment(s.hostname, s.replicas, deploymentStrategy(s.strategy), corev1.PodSpecArgs{
		ServiceAccountName: pulumi.String(s.hostname), TerminationGracePeriodSeconds: pulumi.Int(35), EnableServiceLinks: pulumi.Bool(false),
		InitContainers: corev1.ContainerArray{
			corev1.ContainerArgs{Name: pulumi.String("cloud-sql-proxy"), Image: pulumi.String(s.images.proxy), RestartPolicy: pulumi.String("Always"), Args: pulumi.StringArray{pulumi.String("--private-ip"), pulumi.String("--structured-logs"), pulumi.String("--address=127.0.0.1"), pulumi.String("--port=5432"), cloud.instance.ConnectionName}, Resources: resources("200m", "256Mi"), SecurityContext: &corev1.SecurityContextArgs{RunAsNonRoot: pulumi.Bool(true)}},
		},
		Containers: corev1.ContainerArray{
			corev1.ContainerArgs{Name: pulumi.String("envoy-dispatch"), Image: pulumi.String(s.images.envoy), Command: pulumi.StringArray{pulumi.String("/usr/local/bin/envoy-dispatch")}, Resources: resources("1", "4Gi"), Env: appEnv, VolumeMounts: appMounts, Lifecycle: appPreStop, ReadinessProbe: &corev1.ProbeArgs{Exec: &corev1.ExecActionArgs{Command: pulumi.StringArray{pulumi.String("curl"), pulumi.String("-fsS"), pulumi.String("http://127.0.0.1:8766/healthz")}}, PeriodSeconds: pulumi.Int(10), TimeoutSeconds: pulumi.Int(3), FailureThreshold: pulumi.Int(3), InitialDelaySeconds: pulumi.Int(5)}},
		},
		Volumes: appVolumes,
	}, frontDoor, appGrants); err != nil {
		return nil, err
	}
	if _, err := service(s.hostname, 8766); err != nil {
		return nil, err
	}

	podPeer := func(app string) networkingv1.NetworkPolicyPeerArgs {
		return networkingv1.NetworkPolicyPeerArgs{PodSelector: &metav1.LabelSelectorArgs{MatchLabels: pulumi.StringMap{"app": pulumi.String(app)}}}
	}
	ingressFrom := func(port int, peers networkingv1.NetworkPolicyPeerArray) networkingv1.NetworkPolicyIngressRuleArgs {
		return networkingv1.NetworkPolicyIngressRuleArgs{
			From:  peers,
			Ports: networkingv1.NetworkPolicyPortArray{networkingv1.NetworkPolicyPortArgs{Port: pulumi.Int(port)}},
		}
	}
	policy := func(name string, ingress networkingv1.NetworkPolicyIngressRuleArray) error {
		_, err := networkingv1.NewNetworkPolicy(ctx, name, &networkingv1.NetworkPolicyArgs{
			Metadata: metadata(name),
			Spec: &networkingv1.NetworkPolicySpecArgs{
				PodSelector: &metav1.LabelSelectorArgs{MatchLabels: pulumi.StringMap{"app": pulumi.String(name)}},
				PolicyTypes: pulumi.StringArray{pulumi.String("Ingress")},
				Ingress:     ingress,
			},
		}, child...)
		return err
	}
	// An ingress rule with an empty from admits every source, so a named namespace gets a rule and
	// no named namespace gets none.
	appIngress := networkingv1.NetworkPolicyIngressRuleArray{}
	if len(args.AllowedNamespaces) > 0 {
		namespacePeers := make(networkingv1.NetworkPolicyPeerArray, 0, len(args.AllowedNamespaces))
		for _, namespace := range args.AllowedNamespaces {
			namespacePeers = append(namespacePeers, networkingv1.NetworkPolicyPeerArgs{
				NamespaceSelector: &metav1.LabelSelectorArgs{
					MatchLabels: pulumi.StringMap{"kubernetes.io/metadata.name": pulumi.String(namespace)},
				},
			})
		}
		appIngress = append(appIngress, ingressFrom(8766, namespacePeers))
	}
	// The front door is a pod of this namespace, which no namespace selector covers, so without
	// this rule Serve reaches the app at no port at all.
	appIngress = append(appIngress, ingressFrom(8766, networkingv1.NetworkPolicyPeerArray{podPeer(tailnetName)}))
	if err := policy(s.hostname, appIngress); err != nil {
		return nil, err
	}
	// Whatever reaches the proxy gets tailnet egress as this node, so only the two workloads that
	// dial the hub through it may.
	if err := policy(tailnetName, networkingv1.NetworkPolicyIngressRuleArray{
		ingressFrom(dispatchSOCKS5Port, networkingv1.NetworkPolicyPeerArray{podPeer(s.hostname), podPeer(listenerName)}),
	}); err != nil {
		return nil, err
	}
	if err := policy(listenerName, networkingv1.NetworkPolicyIngressRuleArray{
		ingressFrom(core.DefaultListenerPort, networkingv1.NetworkPolicyPeerArray{podPeer(s.hostname)}),
	}); err != nil {
		return nil, err
	}
	return &dispatchKubernetes{kubeconfig: kubeconfig}, nil
}
