{{- define "bank0.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "bank0.dsnSecretName" -}}
{{- if .Values.database.existingSecret }}{{ .Values.database.existingSecret }}{{- else }}{{ .Release.Name }}-db{{- end -}}
{{- end -}}

{{/* DSN env, sourced from the secret. Call with root context. */}}
{{- define "bank0.dsnEnv" -}}
- name: APP_DATABASE_DSN
  valueFrom:
    secretKeyRef:
      name: {{ include "bank0.dsnSecretName" . }}
      key: {{ .Values.database.secretKey }}
{{- end -}}

{{- define "bank0.authSecretName" -}}
{{- if .Values.auth.existingSecret }}{{ .Values.auth.existingSecret }}{{- else }}{{ .Release.Name }}-auth{{- end -}}
{{- end -}}

{{/* JWT secret env for the client surface, if configured. Call with root context. */}}
{{- define "bank0.jwtEnv" -}}
{{- if or .Values.auth.existingSecret .Values.auth.jwtSecret }}
- name: APP_AUTH_JWT_SECRET
  valueFrom:
    secretKeyRef:
      name: {{ include "bank0.authSecretName" . }}
      key: {{ .Values.auth.secretKey }}
{{- end }}
{{- end -}}
