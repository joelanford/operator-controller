{{- define "olm.targetNamespaces" -}}
{{- $targetNamespaces := .Values.watchNamespace -}}
{{- if not $targetNamespaces -}}
  {{- $targetNamespaces = "" -}}
{{- end -}}
{{- $targetNamespaces -}}
{{- end -}}
