{{- define "casebox.fullname" -}}
{{- printf "%s-casebox" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "casebox.labels" -}}
app.kubernetes.io/name: casebox
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "casebox.selector" -}}
app.kubernetes.io/name: casebox
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "casebox.tokensSecret" -}}
{{- if .Values.existingSecret -}}{{ .Values.existingSecret }}{{- else -}}{{ include "casebox.fullname" . }}-tokens{{- end -}}
{{- end -}}

{{- define "casebox.image" -}}
{{- $tag := .image.tag | default .root.Chart.AppVersion -}}
{{- printf "%s:%s" .image.repository $tag -}}
{{- end -}}

{{- define "casebox.dbPassword" -}}
- name: DB_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ required "database.passwordSecret.name is required" .Values.database.passwordSecret.name }}
      key: {{ .Values.database.passwordSecret.key }}
{{- end -}}
