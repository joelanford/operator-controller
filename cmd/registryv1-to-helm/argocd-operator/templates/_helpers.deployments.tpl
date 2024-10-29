{{- define "deployment.argocd-operator-controller-manager.affinity" -}}
null
{{- end -}}

{{- define "deployment.argocd-operator-controller-manager.kube-rbac-proxy.env" -}}
null
{{- end -}}

{{- define "deployment.argocd-operator-controller-manager.kube-rbac-proxy.envFrom" -}}
null
{{- end -}}

{{- define "deployment.argocd-operator-controller-manager.kube-rbac-proxy.resources" -}}
{}
{{- end -}}

{{- define "deployment.argocd-operator-controller-manager.kube-rbac-proxy.volumeMounts" -}}
null
{{- end -}}

{{- define "deployment.argocd-operator-controller-manager.manager.env" -}}
[{"name":"WATCH_NAMESPACE","valueFrom":{"fieldRef":{"fieldPath":"metadata.annotations['olm.targetNamespaces']"}}}]
{{- end -}}

{{- define "deployment.argocd-operator-controller-manager.manager.envFrom" -}}
null
{{- end -}}

{{- define "deployment.argocd-operator-controller-manager.manager.resources" -}}
{}
{{- end -}}

{{- define "deployment.argocd-operator-controller-manager.manager.volumeMounts" -}}
null
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

