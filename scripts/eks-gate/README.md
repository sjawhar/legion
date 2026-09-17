# EKS rollout gate drivers

These scripts are the executable checks for the production EKS rollout. Run them from the devbox as
uid `legion`, always name the production kubeconfig context explicitly, and paste their `GATE …`
lines into Dispatch `LEGION-19`. A `FAILED` line exits non-zero. No script falls back to the current
kubectl context.

## Gate 2: cluster objects, network, storage, and providers

After Plan C's production apply creates namespace `legion`, the `legion` NodePool, the PriorityClass,
and the security-group rules, run:

```sh
bash scripts/eks-gate/network.sh --context production
```

The driver creates `legion-gate2`, `legion-gate2-default`, and one PVC in namespace `legion`. It
waits for the dedicated-pool PVC to bind, proves that the dedicated pool reaches NATS, the Envoy
listener, Dispatch, and both daemon ports, proves that the default pool is refused everywhere except
Dispatch HTTPS, and confirms the node has both the cluster and `legion-nodes` security groups. Its
exit trap deletes all three test objects.

Create the persistent providers Secret once per daemon. Resolve values inside the `secrets` wrapper
and export the Envoy listener bearer before running the driver:

```sh
secrets ANTHROPIC_API_KEY GEMINI_API_KEY OPENAI_API_KEY DISPATCH_TOKEN -- sh -c '
  export ENVOY_TOKEN="$(aws secretsmanager get-secret-value --region us-west-2 --secret-id production/envoy/api-token --query SecretString --output text | tr -d "\n")"
  bash scripts/eks-gate/secrets.sh --context production --name legion-sjawharlegion-providers
'
```

`secrets.sh` refuses a missing value, pipes its manifest to kubectl rather than putting a value in an
argument, creates no temporary objects, and prints only the five key names.

## Gate 3: pod and volume continuity

For the first running Legion worker pod, run:

```sh
bash scripts/eks-gate/verify-pod.sh --context production <pod>
```

It verifies the Legion pool label, `do-not-disrupt` annotation, toleration, PriorityClass, restricted
container security context, absence of secret-looking environment values, and a Bound tree PVC.

While a tree has live work, run:

```sh
bash scripts/eks-gate/node-loss.sh --context production --daemon-url http://<devbox-vpc-ip>:13370 <tree>
```

This deliberately cordons and drains the tree's Legion node. It records the root locator from the
tree plus every role's session identity before the drain. A replacement must retain each session id,
advance its generation, renew its ready confirmation, run on a new Legion node in the same
Availability Zone, rebind the PVC, and carry no workspace-recovery prompt. It does not uncordon the
drained node: Karpenter replaces it.

## Local driver tests

```sh
bash scripts/eks-gate/network.test.sh
bash scripts/eks-gate/node-loss.test.sh
bash scripts/eks-gate/secrets.test.sh
shellcheck scripts/eks-gate/*.sh
```

The PATH-shim tests include an explicit missing-`--context` refusal. They prove driver control flow;
only the production invocations prove the VPC and EBS facts.
