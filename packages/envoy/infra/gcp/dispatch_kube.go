package gcp

import (
	"errors"
	"fmt"
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

// refuseDispatchSeedInSecret is value, a secret Input of the component, as a secret, with a value
// holding an nkey seed refused once it resolves: the output fails naming input and never the
// value, so no child is registered with it. ApplyT keeps value's secretness, and ToSecret marks
// the result secret whatever value's own marking.
func refuseDispatchSeedInSecret(value pulumi.StringInput, dispatch, input string) pulumi.StringOutput {
	return pulumi.ToSecret(value.ToStringOutput().ApplyT(func(v string) (string, error) {
		if bus.HoldsSeed(v) {
			return "", fmt.Errorf("%s: %w", dispatch, core.SeedRefusal(input))
		}
		return v, nil
	})).(pulumi.StringOutput)
}

// dispatchListenerEnv renders the envoy-listener container's environment from the one NATS URL
// once it resolves. core's Env names its own inputs — natsUrls by index and socks5Proxy, neither
// an input of gcp:Dispatch — so each refusal is mapped onto natsUrl, this component's input. The
// SOCKS5 proxy is fixed (the Tailscale sidecar's), so its refusal means the URL names a local
// server, which the pod's NATS clients can never reach through the tailnet proxy.
func dispatchListenerEnv(hostname, url string) (corev1.EnvVarArray, error) {
	env, err := (core.ListenerConfig{
		MachineID:   hostname,
		Port:        core.DefaultListenerPort,
		SOCKS5Proxy: "127.0.0.1:1055",
	}).Env([]string{url})
	if err != nil {
		var input *core.InputError
		if errors.As(err, &input) && input.Input == "socks5Proxy" {
			return nil, core.InputErrorf("natsUrl", "natsUrl names a local NATS server; the pod reaches NATS only through its Tailscale sidecar's SOCKS5 proxy, so the URL must be remote (the hub's tailnet name)")
		}
		return nil, core.InputErrorf("natsUrl", "%s", strings.ReplaceAll(err.Error(), "natsUrls[0]", "natsUrl"))
	}
	values := make(corev1.EnvVarArray, 0, len(env)+1)
	for _, item := range env {
		name, value, _ := strings.Cut(item, "=")
		values = append(values, valueEnv(name, value))
	}
	values = append(values, valueEnv("NATS_NKEY_SEED_FILE", "/var/run/secrets/envoy/nats-seed"))
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
	if _, err := corev1.NewNamespace(ctx, s.namespace, &corev1.NamespaceArgs{Metadata: &metav1.ObjectMetaArgs{Name: pulumi.String(s.namespace)}}, child...); err != nil {
		return nil, err
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

	// natsURL is the one NATS URL once checked: every container reads this output rather than the
	// raw input, so envoy-dispatch's NATS_URLS is never an unchecked copy beside the listener's
	// checked environment.
	natsURL := args.NatsUrl.ToStringOutput().ApplyT(func(url string) (string, error) {
		if _, err := dispatchListenerEnv(s.hostname, url); err != nil {
			return "", fmt.Errorf("%s: %w", dispatch, err)
		}
		return url, nil
	}).(pulumi.StringOutput)
	listenerEnv := natsURL.ApplyT(func(url string) (corev1.EnvVarArray, error) {
		env, err := dispatchListenerEnv(s.hostname, url)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", dispatch, err)
		}
		return env, nil
	}).(corev1.EnvVarArrayOutput)

	// The pod is ready only once the Auth Proxy can connect (roles/cloudsql.client) and the seed
	// volume can mount (roles/secretmanager.secretAccessor), so the Deployment waits for both
	// grants; registered before them, its readiness wait would race IAM propagation on the first
	// deployment.
	deploymentOptions := make([]pulumi.ResourceOption, 0, len(child)+1)
	deploymentOptions = append(deploymentOptions, child...)
	deploymentOptions = append(deploymentOptions, pulumi.DependsOn([]pulumi.Resource{cloud.sqlClient, cloud.seedAccessor}))

	if _, err := appsv1.NewDeployment(ctx, s.hostname, &appsv1.DeploymentArgs{
		Metadata: metadata(s.hostname),
		Spec: appsv1.DeploymentSpecArgs{
			Replicas: pulumi.Int(1),
			Strategy: &appsv1.DeploymentStrategyArgs{Type: pulumi.String("Recreate")},
			Selector: &metav1.LabelSelectorArgs{MatchLabels: pulumi.StringMap{"app": pulumi.String(s.hostname)}},
			Template: corev1.PodTemplateSpecArgs{
				Metadata: &metav1.ObjectMetaArgs{Labels: pulumi.StringMap{"app": pulumi.String(s.hostname)}},
				Spec: corev1.PodSpecArgs{
					ServiceAccountName: pulumi.String(s.hostname), TerminationGracePeriodSeconds: pulumi.Int(30), EnableServiceLinks: pulumi.Bool(false),
					InitContainers: corev1.ContainerArray{
						corev1.ContainerArgs{Name: pulumi.String("tailscale"), Image: pulumi.String(s.images.tailscale), RestartPolicy: pulumi.String("Always"), Resources: resources("100m", "128Mi"), Env: corev1.EnvVarArray{
							secretEnv("TS_AUTHKEY", authKeyName, "TS_AUTHKEY"),
							valueEnv("TS_AUTH_ONCE", "true"), valueEnv("TS_KUBE_SECRET", stateName), valueEnv("TS_USERSPACE", "true"), valueEnv("TS_SOCKS5_SERVER", "127.0.0.1:1055"), valueEnv("TS_SERVE_CONFIG", "/etc/tailscale/serve.json"), valueEnv("TS_HOSTNAME", s.hostname), valueEnv("TS_EXTRA_ARGS", "--advertise-tags="+strings.Join(s.tags, ",")), valueEnv("TS_ENABLE_HEALTH_CHECK", "true"), valueEnv("TS_LOCAL_ADDR_PORT", "127.0.0.1:9002"),
						}, VolumeMounts: corev1.VolumeMountArray{corev1.VolumeMountArgs{Name: pulumi.String("serve"), MountPath: pulumi.String("/etc/tailscale")}}, StartupProbe: &corev1.ProbeArgs{Exec: &corev1.ExecActionArgs{Command: pulumi.StringArray{pulumi.String("wget"), pulumi.String("-qO-"), pulumi.String("http://127.0.0.1:9002/healthz")}}, PeriodSeconds: pulumi.Int(5), FailureThreshold: pulumi.Int(36)}, ReadinessProbe: &corev1.ProbeArgs{Exec: &corev1.ExecActionArgs{Command: pulumi.StringArray{pulumi.String("wget"), pulumi.String("-qO-"), pulumi.String("http://127.0.0.1:9002/healthz")}}, PeriodSeconds: pulumi.Int(10)}},
						corev1.ContainerArgs{Name: pulumi.String("cloud-sql-proxy"), Image: pulumi.String(s.images.proxy), RestartPolicy: pulumi.String("Always"), Args: pulumi.StringArray{pulumi.String("--private-ip"), pulumi.String("--structured-logs"), pulumi.String("--address=127.0.0.1"), pulumi.String("--port=5432"), cloud.instance.ConnectionName}, Resources: resources("200m", "256Mi"), SecurityContext: &corev1.SecurityContextArgs{RunAsNonRoot: pulumi.Bool(true)}},
					},
					Containers: corev1.ContainerArray{
						corev1.ContainerArgs{Name: pulumi.String("envoy-dispatch"), Image: pulumi.String(s.images.envoy), Command: pulumi.StringArray{pulumi.String("/usr/local/bin/envoy-dispatch")}, Resources: resources("1", "4Gi"), Env: corev1.EnvVarArray{
							valueEnv("HOME", "/home/envoy"), secretEnv("DATABASE_URL", dispatchSecretName, "DATABASE_URL"), secretEnv("DISPATCH_AGENT_TOKEN", dispatchSecretName, "DISPATCH_AGENT_TOKEN"), secretEnv("DISPATCH_SIGNING_KEY", dispatchSecretName, "DISPATCH_SIGNING_KEY"), secretEnv("DISPATCH_SIGNIN_CLIENT_SECRET", dispatchSecretName, "DISPATCH_SIGNIN_CLIENT_SECRET"),
							valueEnv("DISPATCH_SIGNIN_ISSUER", "https://accounts.google.com"), valueEnv("DISPATCH_SIGNIN_CLIENT_ID", s.signIn.clientID), valueEnv("DISPATCH_SIGNIN_DOMAIN", s.signIn.domain), valueEnv("DISPATCH_SERVER_URL", "https://"+s.hostname+"."+s.tailnetDNSName), valueEnv("DISPATCH_LISTEN_HOST", "0.0.0.0"), corev1.EnvVarArgs{Name: pulumi.String("NATS_URLS"), Value: natsURL}, valueEnv("ENVOY_ALLOW_REMOTE_NATS", "1"), valueEnv("NATS_NKEY_SEED_FILE", "/var/run/secrets/envoy/nats-seed"), valueEnv("ENVOY_NATS_SOCKS5_PROXY", "127.0.0.1:1055"), valueEnv("ENVOY_URL", "http://127.0.0.1:9020"),
						}, VolumeMounts: corev1.VolumeMountArray{corev1.VolumeMountArgs{Name: pulumi.String("nats-seed"), MountPath: pulumi.String("/var/run/secrets/envoy"), ReadOnly: pulumi.Bool(true)}}, ReadinessProbe: &corev1.ProbeArgs{Exec: &corev1.ExecActionArgs{Command: pulumi.StringArray{pulumi.String("curl"), pulumi.String("-fsS"), pulumi.String("http://127.0.0.1:8766/healthz")}}, PeriodSeconds: pulumi.Int(10), TimeoutSeconds: pulumi.Int(3), FailureThreshold: pulumi.Int(3), InitialDelaySeconds: pulumi.Int(5)}},
						corev1.ContainerArgs{Name: pulumi.String("envoy-listener"), Image: pulumi.String(s.images.envoy), Resources: resources("200m", "512Mi"), Env: listenerEnv, VolumeMounts: corev1.VolumeMountArray{corev1.VolumeMountArgs{Name: pulumi.String("nats-seed"), MountPath: pulumi.String("/var/run/secrets/envoy"), ReadOnly: pulumi.Bool(true)}}, ReadinessProbe: &corev1.ProbeArgs{Exec: &corev1.ExecActionArgs{Command: pulumi.StringArray{pulumi.String("curl"), pulumi.String("-fsS"), pulumi.String("http://127.0.0.1:9020/healthz")}}}},
					},
					Volumes: corev1.VolumeArray{
						corev1.VolumeArgs{Name: pulumi.String("nats-seed"), Csi: &corev1.CSIVolumeSourceArgs{Driver: pulumi.String("secrets-store-gke.csi.k8s.io"), ReadOnly: pulumi.Bool(true), VolumeAttributes: pulumi.StringMap{"secretProviderClass": pulumi.String(seedName)}}},
						corev1.VolumeArgs{Name: pulumi.String("serve"), ConfigMap: &corev1.ConfigMapVolumeSourceArgs{Name: pulumi.String(serveName)}},
					},
				},
			},
		},
	}, deploymentOptions...); err != nil {
		return nil, err
	}
	if _, err := corev1.NewService(ctx, s.hostname, &corev1.ServiceArgs{
		Metadata: metadata(s.hostname),
		Spec: &corev1.ServiceSpecArgs{
			Selector: pulumi.StringMap{"app": pulumi.String(s.hostname)},
			Ports:    corev1.ServicePortArray{corev1.ServicePortArgs{Port: pulumi.Int(8766), TargetPort: pulumi.Int(8766)}},
		},
	}, child...); err != nil {
		return nil, err
	}
	ingress := networkingv1.NetworkPolicyIngressRuleArray{}
	if len(args.AllowedNamespaces) > 0 {
		namespacePeers := make(networkingv1.NetworkPolicyPeerArray, 0, len(args.AllowedNamespaces))
		for _, namespace := range args.AllowedNamespaces {
			namespacePeers = append(namespacePeers, networkingv1.NetworkPolicyPeerArgs{
				NamespaceSelector: &metav1.LabelSelectorArgs{
					MatchLabels: pulumi.StringMap{"kubernetes.io/metadata.name": pulumi.String(namespace)},
				},
			})
		}
		ingress = append(ingress, networkingv1.NetworkPolicyIngressRuleArgs{
			From:  namespacePeers,
			Ports: networkingv1.NetworkPolicyPortArray{networkingv1.NetworkPolicyPortArgs{Port: pulumi.Int(8766)}},
		})
	}
	if _, err := networkingv1.NewNetworkPolicy(ctx, s.hostname, &networkingv1.NetworkPolicyArgs{
		Metadata: metadata(s.hostname),
		Spec: &networkingv1.NetworkPolicySpecArgs{
			PodSelector: &metav1.LabelSelectorArgs{MatchLabels: pulumi.StringMap{"app": pulumi.String(s.hostname)}},
			PolicyTypes: pulumi.StringArray{pulumi.String("Ingress")},
			// With no allowed namespaces there is no rule at all, so the policy admits no ingress:
			// the Tailscale sidecar only dials out and every probe is an exec probe, so only the
			// namespaces the consumer names need a path to port 8766.
			Ingress: ingress,
		},
	}, child...); err != nil {
		return nil, err
	}
	return &dispatchKubernetes{kubeconfig: kubeconfig}, nil
}
