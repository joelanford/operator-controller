{{- define "deployment.argocd-operator-controller-manager.affinity" -}}
null
{{- end -}}

{{- define "deployment.argocd-operator-controller-manager.kube-rbac-proxy" -}}
{"name":"kube-rbac-proxy","image":"gcr.io/kubebuilder/kube-rbac-proxy:v0.8.0","args":["--secure-listen-address=0.0.0.0:8443","--upstream=http://127.0.0.1:8080/","--logtostderr=true","--v=10"],"ports":[{"name":"https","containerPort":8443}],"resources":{}}
{{- end -}}

{{- define "deployment.argocd-operator-controller-manager.manager" -}}
{"name":"manager","image":"quay.io/argoprojlabs/argocd-operator@sha256:99aeec24cc406d06d18822347d9ac3ed053a702d8419191e4e681075fed7b9bb","command":["/manager"],"args":["--health-probe-bind-address=:8081","--metrics-bind-address=127.0.0.1:8080","--leader-elect"],"env":[{"name":"WATCH_NAMESPACE","valueFrom":{"fieldRef":{"fieldPath":"metadata.annotations['olm.targetNamespaces']"}}}],"resources":{},"livenessProbe":{"httpGet":{"path":"/healthz","port":8081},"initialDelaySeconds":15,"periodSeconds":20},"readinessProbe":{"httpGet":{"path":"/readyz","port":8081},"initialDelaySeconds":5,"periodSeconds":10},"securityContext":{"capabilities":{"drop":["ALL"]},"runAsNonRoot":true,"readOnlyRootFilesystem":true,"allowPrivilegeEscalation":false}}
{{- end -}}

{{- define "deployment.argocd-operator-controller-manager.nodeSelector" -}}
null
{{- end -}}

{{- define "deployment.argocd-operator-controller-manager.selector" -}}
{"matchLabels":{"control-plane":"controller-manager"}}
{{- end -}}

{{- define "deployment.argocd-operator-controller-manager.tolerations" -}}
null
{{- end -}}

{{- define "deployment.argocd-operator-controller-manager.volumes" -}}
null
{{- end -}}

{{- define "deployment.argocd-operator-controller-manager.spec.overrides" -}}
  {{- $overrides := dict -}}

  {{- $templateMetadataOverrides := dict
    "annotations" (dict
      "olm.targetNamespaces" (include "olm.targetNamespaces" .)
    )
  -}}

  {{- $templateSpecOverrides := dict -}}
  {{- $origAffinity := fromYaml (include "deployment.argocd-operator-controller-manager.affinity" .) -}}
  {{- if .Values.affinity -}}
    {{- $_ := set $templateSpecOverrides "affinity" .Values.affinity -}}
  {{- else if $origAffinity -}}
    {{- $_ := set $templateSpecOverrides "affinity" $origAffinity -}}
  {{- end -}}

  {{- $origNodeSelector := fromYaml (include "deployment.argocd-operator-controller-manager.nodeSelector" .) -}}
  {{- if .Values.nodeSelector -}}
    {{- $_ := set $templateSpecOverrides "nodeSelector" .Values.nodeSelector -}}
  {{- else if $origNodeSelector -}}
    {{- $_ := set $templateSpecOverrides "nodeSelector" $origNodeSelector -}}
  {{- end -}}

  {{- $origSelector := fromYaml (include "deployment.argocd-operator-controller-manager.selector" .) -}}
  {{- if .Values.selector -}}
    {{- $_ := set $overrides "selector" .Values.selector -}}
  {{- else if $origSelector -}}
    {{- $_ := set $overrides "selector" $origSelector -}}
  {{- end -}}

  {{- $origTolerations := fromYaml (include "deployment.argocd-operator-controller-manager.tolerations" .) -}}
  {{- if and $origTolerations .Values.tolerations -}}
    {{- $_ := set $templateSpecOverrides "tolerations" (concat $origTolerations .Values.tolerations | uniq) -}}
  {{- else if .Values.tolerations -}}
    {{- $_ := set $templateSpecOverrides "tolerations" .Values.tolerations -}}
  {{- else if $origTolerations -}}
    {{- $_ := set $templateSpecOverrides "tolerations" $origTolerations -}}
  {{- end -}}

  {{- $origVolumes := fromYaml (include "deployment.argocd-operator-controller-manager.volumes" .) -}}
  {{- if and $origVolumes .Values.volumes -}}
    {{- $volumes := .Values.volumes -}}
    {{- $volumeNames := list -}}
    {{- range $volumes -}}{{- $volumeNames = append $volumeNames .name -}}{{- end -}}
    {{- range $origVolumes -}}
      {{- if not (has .name $volumeNames) -}}
        {{- $volumes = append $volumes . -}}
        {{- $volumeNames = append $volumeNames .name -}}
      {{- end -}}
    {{- end -}}
    {{- $_ := set $templateSpecOverrides "volumes" $volumes -}}
  {{- else if .Values.volumes -}}
    {{- $_ := set $templateSpecOverrides "volumes" .Values.volumes -}}
  {{- else if $origVolumes -}}
    {{- $_ := set $templateSpecOverrides "volumes" $origVolumes -}}
  {{- end -}}

  {{- $containers := list -}}


  {{- $origContainer0 := fromYaml (include "deployment.argocd-operator-controller-manager.kube-rbac-proxy" .) -}}

  {{- $origEnv0 := $origContainer0.env -}}
  {{- if and $origEnv0 .Values.env -}}
    {{- $env := .Values.env -}}
    {{- $envNames := list -}}
    {{- range $env -}}{{- $envNames = append $envNames .name -}}{{- end -}}
    {{- range $origEnv0 -}}
      {{- if not (has .name $envNames) -}}
        {{- $env = append $env . -}}
        {{- $envNames = append $envNames .name -}}
      {{- end -}}
    {{- end -}}
    {{- $_ := set $origContainer0 "env" $env -}}
  {{- else if .Values.env -}}
    {{- $_ := set $origContainer0 "env" .Values.env -}}
  {{- end -}}

  {{- $origEnvFrom0 := $origContainer0.envFrom -}}
  {{- if and $origEnvFrom0 .Values.envFrom -}}
    {{- $_ := set $origContainer0 "envFrom" (concat $origEnvFrom0 .Values.envFrom | uniq) -}}
  {{- else if .Values.envFrom -}}
    {{- $_ := set $origContainer0 "envFrom" .Values.envFrom -}}
  {{- end -}}

  {{- $origResources0 := $origContainer0.resources -}}
  {{- if .Values.resources -}}
    {{- $_ := set $origContainer0 "resources" .Values.resources -}}
  {{- end -}}

  {{- $origVolumeMounts0 := $origContainer0.volumeMounts -}}
  {{- if and $origVolumeMounts0 .Values.volumeMounts -}}
    {{- $volumeMounts := .Values.volumeMounts -}}
    {{- $volumeMountNames := list -}}
    {{- range $volumeMounts -}}{{- $volumeMountNames = append $volumeMountNames .name -}}{{- end -}}
    {{- range $origVolumeMounts0 -}}
      {{- if not (has .name $volumeMountNames) -}}
        {{- $volumeMounts = append $volumeMounts . -}}
        {{- $volumeMountNames = append $volumeMountNames .name -}}
      {{- end -}}
    {{- end -}}
    {{- $_ := set $origContainer0 "volumeMounts" $volumeMounts -}}
  {{- else if .Values.volumeMounts -}}
    {{- $_ := set $origContainer0 "volumeMounts" .Values.volumeMounts -}}
  {{- end -}}

  {{- $containers = append $containers $origContainer0 -}}

  {{- $origContainer1 := fromYaml (include "deployment.argocd-operator-controller-manager.manager" .) -}}

  {{- $origEnv1 := $origContainer1.env -}}
  {{- if and $origEnv1 .Values.env -}}
    {{- $env := .Values.env -}}
    {{- $envNames := list -}}
    {{- range $env -}}{{- $envNames = append $envNames .name -}}{{- end -}}
    {{- range $origEnv1 -}}
      {{- if not (has .name $envNames) -}}
        {{- $env = append $env . -}}
        {{- $envNames = append $envNames .name -}}
      {{- end -}}
    {{- end -}}
    {{- $_ := set $origContainer1 "env" $env -}}
  {{- else if .Values.env -}}
    {{- $_ := set $origContainer1 "env" .Values.env -}}
  {{- end -}}

  {{- $origEnvFrom1 := $origContainer1.envFrom -}}
  {{- if and $origEnvFrom1 .Values.envFrom -}}
    {{- $_ := set $origContainer1 "envFrom" (concat $origEnvFrom1 .Values.envFrom | uniq) -}}
  {{- else if .Values.envFrom -}}
    {{- $_ := set $origContainer1 "envFrom" .Values.envFrom -}}
  {{- end -}}

  {{- $origResources1 := $origContainer1.resources -}}
  {{- if .Values.resources -}}
    {{- $_ := set $origContainer1 "resources" .Values.resources -}}
  {{- end -}}

  {{- $origVolumeMounts1 := $origContainer1.volumeMounts -}}
  {{- if and $origVolumeMounts1 .Values.volumeMounts -}}
    {{- $volumeMounts := .Values.volumeMounts -}}
    {{- $volumeMountNames := list -}}
    {{- range $volumeMounts -}}{{- $volumeMountNames = append $volumeMountNames .name -}}{{- end -}}
    {{- range $origVolumeMounts1 -}}
      {{- if not (has .name $volumeMountNames) -}}
        {{- $volumeMounts = append $volumeMounts . -}}
        {{- $volumeMountNames = append $volumeMountNames .name -}}
      {{- end -}}
    {{- end -}}
    {{- $_ := set $origContainer1 "volumeMounts" $volumeMounts -}}
  {{- else if .Values.volumeMounts -}}
    {{- $_ := set $origContainer1 "volumeMounts" .Values.volumeMounts -}}
  {{- end -}}

  {{- $containers = append $containers $origContainer1 -}}
  {{- $templateSpecOverrides := merge $templateSpecOverrides (dict "containers" $containers) -}}

  {{- $overrides = merge $overrides (dict "template" (dict "metadata" $templateMetadataOverrides)) -}}
  {{- $overrides = merge $overrides (dict "template" (dict "spec" $templateSpecOverrides)) -}}
  {{- dict "spec" $overrides | toYaml -}}
{{- end -}}