{{/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT License.
*/}}
{{- $config := deepCopy .Values.openclaw.config }}
{{- /* Use the desktop's headed browser when available; otherwise let OpenClaw
   launch a headless browser. Explicit boolean preferences take precedence. */}}
{{- $browser := dig "browser" (dict) $config | default dict }}
{{- if eq (index $browser "attachOnly") nil }}
{{- $_ := set $browser "attachOnly" .Values.desktop.enabled }}
{{- end }}
{{- if eq (index $browser "headless") nil }}
{{- $_ := set $browser "headless" (not .Values.desktop.enabled) }}
{{- end }}
{{- $_ := set $config "browser" $browser }}
{{- if and .Values.sandboxWorkload (hasKey .Values.sandboxWorkload "trustedProxyIPs") }}
{{- $gateway := dig "gateway" (dict) $config }}
{{- $_ := set $gateway "trustedProxies" (.Values.sandboxWorkload.trustedProxyIPs | default list) }}
{{- $_ := set $config "gateway" $gateway }}
{{- end }}
{{- /* CetiRun owns Tailscale Serve outside OpenClaw and forwards it to the
   ordinary Gateway listener over loopback. OpenClaw requires the immediate
   proxy source to be trusted before applying normal token and device auth. */}}
{{- if .TailscaleEnabled }}
{{- $gateway := dig "gateway" (dict) $config }}
{{- $trustedProxies := dig "trustedProxies" (list) $gateway }}
{{- if not (has "127.0.0.1" $trustedProxies) }}
{{- $_ := set $gateway "trustedProxies" (append $trustedProxies "127.0.0.1") }}
{{- end }}
{{- $_ := set $config "gateway" $gateway }}
{{- end }}
{{- /* Portal sign-in: the ingress lets only the Agent's owner through, after
   their Portal sign-in, and passes their email from that sign-in. The only
   trusted proxy is the Istio sidecar or the managed in-Pod mTLS endpoint.
   OpenClaw takes the owner from that header and approves their browsers. The
   gateway token becomes a password for clients on loopback, such as the CLI
   in the Terminal, as OpenClaw refuses a token in this mode. */}}
{{- if and (dig "portalSignIn" "enabled" false .Values.openclaw) (or (not .Values.sandboxWorkload) (dig "ingressMTLS" false (.Values.sandboxWorkload | default dict))) (not .TailscaleEnabled) }}
{{- $gateway := dig "gateway" (dict) $config }}
{{- /* OpenClaw falls back to the remote token even when gateway.mode is local. */}}
{{- $_ := unset (dig "remote" (dict) $gateway | default dict) "token" }}
{{- $trustedProxies := dig "trustedProxies" (list) $gateway }}
{{- if .Values.sandboxWorkload }}
{{- if not (and (eq (len $trustedProxies) 1) (has "127.0.0.1" $trustedProxies)) }}
{{- $_ := required "Sandbox openclaw.portalSignIn needs exactly 127.0.0.1, the managed ingress mTLS proxy, in sandboxWorkload.trustedProxyIPs" "" }}
{{- end }}
{{- if ne (dig "bind" "" $gateway) "loopback" }}
{{- $_ := required "Sandbox openclaw.portalSignIn needs gateway.bind=loopback behind managed ingress mTLS" "" }}
{{- end }}
{{- else if not (has "127.0.0.6" $trustedProxies) }}
{{- $_ := required "openclaw.portalSignIn needs 127.0.0.6, the Istio sidecar, in openclaw.config.gateway.trustedProxies" "" }}
{{- end }}
{{- $_ := set $gateway "auth" (dict
    "mode" "trusted-proxy"
    "trustedProxy" (dict
      "userHeader" "x-auth-request-email"
      "allowLoopback" true
      "deviceAutoApprove" (dict "enabled" true))) }}
{{- $_ := set $config "gateway" $gateway }}
{{- end }}
{{- /* The AI Gateway is the only platform model provider. */}}
{{- $model := required "secrets.model must be set by the CetiRun operator" .Values.secrets.model }}
{{- $gatewayRouted := true }}
{{- $nativeGateway := true }}
{{- $_ := set $config "auth" (dict "profiles" (dict)) }}
{{- $providerPrefix := "openai" }}
{{- $modelFull := printf "openai/%s" $model }}
{{- /* Models the platform AI Gateway offers this Agent. Each becomes a model
   of the Gateway provider, with its limits, and is allowed; the rest of the
   configuration keeps naming the default model. */}}
{{- $gatewayModels := list }}
{{- if $gatewayRouted }}
{{- $gatewayModels = dig "models" (list) .Values.aiGateway }}
{{- end }}
{{- /* Default model and compaction settings */}}
{{- $compaction := dict "memoryFlush" (dict "enabled" true "softThresholdTokens" (int 4000)) }}
{{- $modelConfig := dict }}
{{- $agentsCurrent := dig "agents" (dict) $config }}
{{- $defaultsCurrent := dig "defaults" (dict) $agentsCurrent }}
{{- $_ := set $defaultsCurrent "compaction" $compaction }}
{{- if $gatewayModels }}
{{- $modelsDict := dict }}
{{- $allow := list }}
{{- range $gatewayModels }}
{{- $ref := printf "%s/%s" $providerPrefix .id }}
{{- $_ := set $modelsDict $ref (dict) }}
{{- $allow = append $allow $ref }}
{{- end }}
{{- if not (hasKey $modelsDict $modelFull) }}
{{- $_ := set $modelsDict $modelFull $modelConfig }}
{{- $allow = append $allow $modelFull }}
{{- end }}
{{- $_ := set $defaultsCurrent "model" (dict "primary" $modelFull) }}
{{- $_ := set $defaultsCurrent "models" $modelsDict }}
{{- $_ := set $defaultsCurrent "modelPolicy" (dict "allow" $allow) }}
{{- else }}
{{- $_ := set $defaultsCurrent "model" (dict "primary" $modelFull) }}
{{- $_ := set $defaultsCurrent "models" (dict $modelFull $modelConfig) }}
{{- $_ := set $defaultsCurrent "modelPolicy" (dict "allow" (list $modelFull)) }}
{{- end }}
{{- if $gatewayRouted }}
{{- /* OpenClaw sends short internal tasks, such as session titles and
   activity recaps, to a utility model. Unset, it derives the small model of
   the primary model's provider, OpenAI's for the Gateway provider, which
   the Gateway does not offer, so those requests fail. The Agent's default
   model serves them instead. */}}
{{- $_ := set $defaultsCurrent "utilityModel" $modelFull }}
{{- end }}
{{- $_ := set $agentsCurrent "defaults" $defaultsCurrent }}
{{- $_ := set $config "agents" $agentsCurrent }}
{{- /* Custom API base URL → models.providers.<provider>.baseUrl */}}
{{- if .Values.secrets.apiBase }}
{{- $providerModels := list }}
{{- $listed := false }}
{{- range $gatewayModels }}
{{- $entry := dict "id" .id "name" (.name | default .id) }}
{{- if .contextWindow }}
{{- $_ := set $entry "contextWindow" (int .contextWindow) }}
{{- end }}
{{- if .maxTokens }}
{{- $_ := set $entry "maxTokens" (int .maxTokens) }}
{{- end }}
{{- /* A model the Gateway reports as accepting images gets them with a
   request. Without input, OpenClaw takes the value from its own catalog,
   which knows only some model names; for the rest, another model describes
   images. */}}
{{- if has .id (dig "visionModels" (list) $.Values.aiGateway) }}
{{- $_ := set $entry "input" (list "text" "image") }}
{{- end }}
{{- if eq .id $model }}
{{- $listed = true }}
{{- end }}
{{- $providerModels = append $providerModels $entry }}
{{- end }}
{{- if not $listed }}
{{- $providerModels = append $providerModels (dict "id" $model "name" $model) }}
{{- end }}
{{- $providerEntry := dict "baseUrl" .Values.secrets.apiBase "models" $providerModels }}
{{- if $gatewayRouted }}
{{- /* Bind the Gateway provider to the Agent's virtual key, which the chart
   passes in OPENAI_API_KEY. OpenClaw otherwise prefers a stored openai
   profile over that variable, and an upgraded home may keep the owner's
   own: it would be sent to the Gateway instead. */}}
{{- $_ := set $providerEntry "auth" "api-key" }}
{{- $_ := set $providerEntry "apiKey" (dict "source" "env" "provider" "default" "id" "OPENAI_API_KEY") }}
{{- end }}
{{- $models := dig "models" (dict) $config }}
{{- if $nativeGateway }}
{{- $models = dict }}
{{- end }}
{{- $providers := dig "providers" (dict) $models }}
{{- $_ := set $providers $providerPrefix $providerEntry }}
{{- $_ := set $models "providers" $providers }}
{{- $_ := set $config "models" $models }}
{{- end }}
{{- /* Force tools.profile=full so coding/system tools (Bash, Read, Edit, etc.)
   are always available. Newer OpenClaw defaults to "messaging" which strips them. */}}
{{- $tools := dig "tools" (dict) $config }}
{{- $_ := set $tools "profile" "full" }}
{{- $sessions := dig "sessions" (dict) $tools }}
{{- $_ := set $sessions "visibility" "all" }}
{{- $_ := set $tools "sessions" $sessions }}
{{- $_ := set $config "tools" $tools }}
{{- /* Enable web search when Brave or Perplexity API key is configured */}}
{{- $webSearchProvider := "" }}
{{- if or .Values.secrets.braveApiKey .Values.secrets.perplexityApiKey }}
{{- $webSearchProvider = "brave" }}
{{- if .Values.secrets.perplexityApiKey }}
{{- $webSearchProvider = "perplexity" }}
{{- end }}
{{- $webSearch := dict "enabled" true "provider" $webSearchProvider }}
{{- $tools := dig "tools" (dict) $config }}
{{- $web := dig "web" (dict) $tools }}
{{- $_ := set $web "search" $webSearch }}
{{- $_ := set $tools "web" $web }}
{{- $_ := set $config "tools" $tools }}
{{- end }}
{{- $pluginEntries := dig "plugins" "entries" (dict) $config }}
{{- $pluginsAllow := dig "plugins" "allow" (list) $config }}
{{- /* GitHub link previews and document reads moved from core into the bundled
   github plugin. Keep the prior behavior unless the user explicitly disables it. */}}
{{- if not (hasKey $pluginEntries "github") }}
{{- $_ := set $pluginEntries "github" (dict "enabled" true) }}
{{- end }}
{{- if not (has "github" $pluginsAllow) }}
{{- $pluginsAllow = append $pluginsAllow "github" }}
{{- end }}
{{- if $webSearchProvider }}
{{- $_ := set $pluginEntries $webSearchProvider (dict "enabled" true) }}
{{- if not (has $webSearchProvider $pluginsAllow) }}
{{- $pluginsAllow = append $pluginsAllow $webSearchProvider }}
{{- end }}
{{- end }}
{{- if or .Values.secrets.telegramBotToken .Values.secrets.telegramTokenFile }}
{{- $_ := set $pluginEntries "telegram" (dict "enabled" true) }}
{{- if not (has "telegram" $pluginsAllow) }}
{{- $pluginsAllow = append $pluginsAllow "telegram" }}
{{- end }}
{{- end }}
{{- if and .Values.secrets.slackBotToken .Values.secrets.slackAppToken }}
{{- $_ := set $pluginEntries "slack" (dict "enabled" true) }}
{{- if not (has "slack" $pluginsAllow) }}
{{- $pluginsAllow = append $pluginsAllow "slack" }}
{{- end }}
{{- end }}
{{- if .Values.secrets.discordBotToken }}
{{- $_ := set $pluginEntries "discord" (dict "enabled" true) }}
{{- if not (has "discord" $pluginsAllow) }}
{{- $pluginsAllow = append $pluginsAllow "discord" }}
{{- end }}
{{- end }}
{{- if and .Values.secrets.feishuAppId .Values.secrets.feishuAppSecret }}
{{- $_ := set $pluginEntries "feishu" (dict "enabled" true) }}
{{- if not (has "feishu" $pluginsAllow) }}
{{- $pluginsAllow = append $pluginsAllow "feishu" }}
{{- end }}
{{- end }}
{{- /* Enable ACPX plugin when acpx.enabled is true */}}
{{- if .Values.openclaw.acpx.enabled }}
{{- $acpxConfig := dict
  "permissionMode" .Values.openclaw.acpx.permissionMode
  "nonInteractivePermissions" .Values.openclaw.acpx.nonInteractivePermissions
}}
{{- if .Values.openclaw.acpx.agents }}
{{- $_ := set $acpxConfig "agents" .Values.openclaw.acpx.agents }}
{{- end }}
{{- $_ := set $pluginEntries "acpx" (dict "enabled" true "config" $acpxConfig) }}
{{- /* Explicitly allow the acpx plugin to suppress non-bundled plugin warnings */}}
{{- if not (has "acpx" $pluginsAllow) }}
{{- $pluginsAllow = append $pluginsAllow "acpx" }}
{{- end }}
{{- $acp := dict
  "enabled" true
  "dispatch" (dict "enabled" .Values.openclaw.acpx.dispatch.enabled)
  "backend" .Values.openclaw.acpx.backend
  "defaultAgent" .Values.openclaw.acpx.defaultAgent
  "allowedAgents" .Values.openclaw.acpx.allowedAgents
}}
{{- $_ := set $config "acp" $acp }}
{{- end }}
{{- if $pluginEntries }}
{{- $plugins := dig "plugins" (dict) $config }}
{{- $_ := set $plugins "entries" $pluginEntries }}
{{- if $pluginsAllow }}
{{- $_ := set $plugins "allow" $pluginsAllow }}
{{- end }}
{{- $_ := set $config "plugins" $plugins }}
{{- end }}
{{- /* Fix Telegram connectivity on dual-stack (IPv4+IPv6) clusters.
   Node 22+ enables Happy Eyeballs (autoSelectFamily) by default, which
   causes ENETUNREACH when IPv6 is configured but unreachable. Disable
   autoSelectFamily at the config level so it applies to both gateway
   and embedded agent modes. */}}
{{- $channels := dig "channels" (dict) $config }}
{{- if or .Values.secrets.telegramBotToken .Values.secrets.telegramTokenFile (hasKey $channels "telegram") }}
{{- $telegram := dig "telegram" (dict) $channels }}
{{- $network := dig "network" (dict) $telegram }}
{{- $_ := set $network "autoSelectFamily" false }}
{{- $_ := set $telegram "network" $network }}
{{- /* Support tokenFile for Telegram (reads bot token from file instead of env var) */}}
{{- if .Values.secrets.telegramTokenFile }}
{{- $_ := set $telegram "tokenFile" .Values.secrets.telegramTokenFile }}
{{- end }}
{{- $_ := set $channels "telegram" $telegram }}
{{- end }}
{{- /* Set groupPolicy=open for Slack when Slack plugin is configured */}}
{{- if or (and .Values.secrets.slackBotToken .Values.secrets.slackAppToken) (dig "plugins" "slack" "enabled" false .Values.openclaw) }}
{{- $slack := dig "slack" (dict) $channels }}
{{- $_ := set $slack "groupPolicy" "open" }}
{{- $_ := set $channels "slack" $slack }}
{{- end }}
{{- if $channels }}
{{- $_ := set $config "channels" $channels }}
{{- end }}
{{- $emptySkills := eq (dig "skillsMode" "" (.Values.sandboxWorkload | default dict)) "empty" }}
{{- $contextSkills := and (or (not .Values.sandboxWorkload) (dig "reconciled" false (.Values.sandboxWorkload | default dict))) (dig "enabled" false (.Values.memoryContext | default dict)) }}
{{- $directSkills := or .Values.skills.shared $emptySkills }}
{{- if or $contextSkills $directSkills }}
{{- $skills := dig "skills" (dict) $config }}
{{- $load := dig "load" (dict) $skills }}
{{- if $contextSkills }}
{{- $_ := set $load "extraDirs" (append (dig "extraDirs" (list) $load) "/home/vibe/.agents/skills" | uniq) }}
{{- $_ := set $load "extraDirs" (append (dig "extraDirs" (list) $load) "/opt/cetirun-memory/skills" | uniq) }}
{{- end }}
{{- if $directSkills }}
{{- $_ := set $load "extraDirs" (append (dig "extraDirs" (list) $load) .Values.skills.mountPath | uniq) }}
{{- end }}
{{- $_ := set $load "watch" true }}
{{- $_ := set $skills "load" $load }}
{{- $_ := set $config "skills" $skills }}
{{- end }}
{{- if .Values.skills.npm }}
{{- $skills := dig "skills" (dict) $config }}
{{- $load := dig "load" (dict) $skills }}
{{- $existingDirs := dig "extraDirs" (list) $load }}
{{- $npmDirs := .SkillDirs }}
{{- $_ := set $load "extraDirs" (concat $existingDirs $npmDirs) }}
{{- $_ := set $load "watch" true }}
{{- $_ := set $skills "load" $load }}
{{- $_ := set $config "skills" $skills }}
{{- end }}
{{- /* Add meta block so the config observer does not flag this as anomalous.
   OpenClaw uses meta.lastTouchedVersion to track the last writer. */}}
{{- if $contextSkills }}
{{- $plugins := dig "plugins" (dict) $config }}
{{- $pluginLoad := dig "load" (dict) $plugins }}
{{- $_ := set $pluginLoad "paths" (append (dig "paths" (list) $pluginLoad) "/opt/cetirun-memory/adapters/openclaw" | uniq) }}
{{- $_ := set $plugins "load" $pluginLoad }}
{{- $_ := set $plugins "allow" (append (dig "allow" (list) $plugins) "cetirun-memory-context" | uniq) }}
{{- $entries := dig "entries" (dict) $plugins }}
{{- $memoryPlugin := deepCopy (dig "cetirun-memory-context" (dict) $entries) }}
{{- $memoryHooks := dig "hooks" (dict) $memoryPlugin }}
{{- if or (and (hasKey $plugins "enabled") (not $plugins.enabled)) (and (hasKey $memoryPlugin "enabled") (not $memoryPlugin.enabled)) (and (hasKey $memoryHooks "allowConversationAccess") (not $memoryHooks.allowConversationAccess)) (and (hasKey $memoryHooks "allowPromptInjection") (not $memoryHooks.allowPromptInjection)) }}
{{- $_ := required "Memory Context conflicts with an explicitly disabled OpenClaw plugin or hook" "" }}
{{- end }}
{{- $_ := set $memoryHooks "allowConversationAccess" true }}
{{- $_ := set $memoryPlugin "hooks" $memoryHooks }}
{{- $_ := set $memoryPlugin "enabled" true }}
{{- $_ := set $entries "cetirun-memory-context" $memoryPlugin }}
{{- $_ := set $plugins "entries" $entries }}
{{- $_ := set $config "plugins" $plugins }}
{{- end }}
{{- $meta := dig "meta" (dict) $config }}
{{- $_ := set $meta "lastTouchedVersion" (.Chart.Version | default "unknown") }}
{{- $_ := set $config "meta" $meta }}
{{- if .Values.connectionsMCP.enabled }}
{{- $mcp := dig "mcp" (dict) $config }}
{{- $servers := dig "servers" (dict) $mcp }}
{{- $_ := set $servers "cetirun-connections" (dict "url" .Values.connectionsMCP.url "transport" "streamable-http" "sslVerify" true) }}
{{- $_ := set $mcp "servers" $servers }}
{{- $_ := set $config "mcp" $mcp }}
{{- else }}
{{- $servers := dig "mcp" "servers" (dict) $config }}
{{- $_ := unset $servers "cetirun-connections" }}
{{- end }}
{{ $config | toPrettyJson }}
