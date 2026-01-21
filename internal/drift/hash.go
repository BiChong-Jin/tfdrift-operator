package drift

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// ---- Deployment hashing ----

// A minimal, stable view of a Deployment spec.
// Only include fields you care about detecting drift for.
type DeploymentFingerprint struct {
	Replicas *int32                    `json:"replicas,omitempty"`
	Strategy appsv1.DeploymentStrategy `json:"strategy,omitempty"`
	Template PodTemplateFingerprint    `json:"template"`
}

type PodTemplateFingerprint struct {
	Labels      map[string]string      `json:"labels,omitempty"`
	Annotations map[string]string      `json:"annotations,omitempty"`
	Containers  []ContainerFingerprint `json:"containers"`
}

type ContainerFingerprint struct {
	Name      string                      `json:"name"`
	Image     string                      `json:"image"`
	Env       []EnvVarFingerprint         `json:"env,omitempty"`
	Ports     []ContainerPortFingerprint  `json:"ports,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

type EnvVarFingerprint struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
	// Note: we intentionally ignore ValueFrom for MVP (or handle it later).
}

type ContainerPortFingerprint struct {
	Name          string          `json:"name,omitempty"`
	ContainerPort int32           `json:"containerPort"`
	Protocol      corev1.Protocol `json:"protocol,omitempty"`
}

func HashDeployment(dep *appsv1.Deployment) (string, error) {
	fp := DeploymentFingerprint{
		Replicas: dep.Spec.Replicas,
		Strategy: dep.Spec.Strategy,
		Template: fingerprintPodTemplate(dep.Spec.Template),
	}

	// Canonicalize (stable ordering)
	canonicalizeMap(fp.Template.Labels)
	canonicalizeMap(fp.Template.Annotations)
	sort.Slice(fp.Template.Containers, func(i, j int) bool {
		return fp.Template.Containers[i].Name < fp.Template.Containers[j].Name
	})
	for i := range fp.Template.Containers {
		sort.Slice(fp.Template.Containers[i].Env, func(a, b int) bool {
			return fp.Template.Containers[i].Env[a].Name < fp.Template.Containers[i].Env[b].Name
		})
		sort.Slice(fp.Template.Containers[i].Ports, func(a, b int) bool {
			pa := fp.Template.Containers[i].Ports[a].ContainerPort
			pb := fp.Template.Containers[i].Ports[b].ContainerPort
			if pa == pb {
				return fp.Template.Containers[i].Ports[a].Name < fp.Template.Containers[i].Ports[b].Name
			}
			return pa < pb
		})
	}

	return hashJSON(fp)
}

func fingerprintPodTemplate(t corev1.PodTemplateSpec) PodTemplateFingerprint {
	containers := make([]ContainerFingerprint, 0, len(t.Spec.Containers))
	for _, c := range t.Spec.Containers {
		env := make([]EnvVarFingerprint, 0, len(c.Env))
		for _, e := range c.Env {
			// MVP: ignore valueFrom for simplicity
			env = append(env, EnvVarFingerprint{Name: e.Name, Value: e.Value})
		}

		ports := make([]ContainerPortFingerprint, 0, len(c.Ports))
		for _, p := range c.Ports {
			ports = append(ports, ContainerPortFingerprint{
				Name:          p.Name,
				ContainerPort: p.ContainerPort,
				Protocol:      p.Protocol,
			})
		}

		containers = append(containers, ContainerFingerprint{
			Name:      c.Name,
			Image:     c.Image,
			Env:       env,
			Ports:     ports,
			Resources: c.Resources,
		})
	}

	return PodTemplateFingerprint{
		Labels:      copyMap(t.Labels),
		Annotations: copyMap(t.Annotations),
		Containers:  containers,
	}
}

// ---- Service hashing ----

type ServiceFingerprint struct {
	Type     corev1.ServiceType       `json:"type"`
	Selector map[string]string        `json:"selector,omitempty"`
	Ports    []ServicePortFingerprint `json:"ports"`
}

type ServicePortFingerprint struct {
	Name       string          `json:"name,omitempty"`
	Protocol   corev1.Protocol `json:"protocol,omitempty"`
	Port       int32           `json:"port"`
	TargetPort string          `json:"targetPort"`
	NodePort   int32           `json:"nodePort,omitempty"`
}

func HashService(svc *corev1.Service) (string, error) {
	fp := ServiceFingerprint{
		Type:     svc.Spec.Type,
		Selector: copyMap(svc.Spec.Selector),
		Ports:    make([]ServicePortFingerprint, 0, len(svc.Spec.Ports)),
	}

	for _, p := range svc.Spec.Ports {
		fp.Ports = append(fp.Ports, ServicePortFingerprint{
			Name:       p.Name,
			Protocol:   p.Protocol,
			Port:       p.Port,
			TargetPort: p.TargetPort.String(),
			NodePort:   p.NodePort,
		})
	}

	canonicalizeMap(fp.Selector)
	sort.Slice(fp.Ports, func(i, j int) bool {
		if fp.Ports[i].Port == fp.Ports[j].Port {
			return fp.Ports[i].Name < fp.Ports[j].Name
		}
		return fp.Ports[i].Port < fp.Ports[j].Port
	})

	return hashJSON(fp)
}

// ---- helpers ----

func hashJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func canonicalizeMap(m map[string]string) {
	// Nothing to do for JSON maps themselves, but we can normalize whitespace.
	for k, v := range m {
		m[k] = strings.TrimSpace(v)
	}
}
