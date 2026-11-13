<script lang="ts">
	import { createQuery } from '@tanstack/svelte-query';

	import FormInput from '#lib/components/form/form-input.svelte';
	import * as Alert from '#lib/components/ui/alert/index.js';
	import * as Collapsible from '#lib/components/ui/collapsible/index.js';
	import { ArrowRightIcon, InfoIcon } from '#lib/icons/index.js';
	import { m } from '#lib/paraglide/messages.js';
	import { queryKeys } from '#lib/query/query-keys.js';
	import { systemService } from '#lib/services/system-service.js';
	import type { RailpackBuildInfo } from '#lib/types/gitops.js';
	import { hasPermission } from '#lib/utils/auth.js';
	import { usesContainerdImageStore } from '#lib/utils/docker.js';

	import type { GitOpsSyncFormState } from './gitops-sync-form.svelte';

	type Props = {
		form: GitOpsSyncFormState;
		environmentId: string;
		railpackInfo?: RailpackBuildInfo;
	};

	let { form, environmentId, railpackInfo }: Props = $props();

	const inputs = $derived(form.inputs);

	// A warning only: the backend refuses Railpack builds on Docker's classic image store.
	const dockerInfoQuery = createQuery(() => ({
		queryKey: queryKeys.system.dockerInfo(environmentId),
		queryFn: () => systemService.getDockerInfoForEnvironment(environmentId),
		enabled: hasPermission('system:read', environmentId),
		retry: false
	}));
	const missingContainerdStore = $derived(!!dockerInfoQuery.data && !usesContainerdImageStore(dockerInfoQuery.data));
</script>

{#if missingContainerdStore}
	<Alert.Root variant="warning-subtle" size="sm">
		<InfoIcon class="size-4" />
		<Alert.Title>{m.containerd_image_store_required_title()}</Alert.Title>
		<Alert.Description>{m.containerd_image_store_required_description()}</Alert.Description>
	</Alert.Root>
{/if}

<FormInput
	label={m.source_directory()}
	type="text"
	placeholder="apps/web"
	helpText={m.source_directory_help()}
	bind:input={inputs.sourcePath}
/>

<div class="grid gap-3 sm:grid-cols-2">
	<FormInput
		label={m.ports_container_port()}
		type="number"
		placeholder="3000"
		helpText={m.app_port_help()}
		bind:input={inputs.appPort}
	/>
	<FormInput
		label={m.ports_host_port()}
		type="number"
		placeholder="0"
		helpText={m.published_port_help()}
		bind:input={inputs.hostPort}
	/>
</div>

<Collapsible.Root class="group/collapsible">
	<Collapsible.Trigger>
		{#snippet child({ props })}
			<button
				{...props}
				type="button"
				class="flex w-full items-center justify-between gap-3 py-1 text-left text-sm font-medium text-muted-foreground transition-colors hover:text-foreground"
			>
				{m.build_and_start_commands()}
				<ArrowRightIcon class="size-4 shrink-0 transition-transform group-data-[state=open]/collapsible:rotate-90" />
			</button>
		{/snippet}
	</Collapsible.Trigger>
	<Collapsible.Content>
		<div class="grid gap-3 pt-3 sm:grid-cols-2">
			<FormInput
				label={m.build_command()}
				type="text"
				placeholder="npm run build"
				helpText={m.railpack_command_override_help()}
				bind:input={inputs.buildCommand}
			/>
			<FormInput
				label={m.start_command()}
				type="text"
				placeholder="npm start"
				helpText={m.railpack_command_override_help()}
				bind:input={inputs.startCommand}
			/>
		</div>
	</Collapsible.Content>
</Collapsible.Root>

{#if railpackInfo}
	<dl class="grid gap-1 rounded-md border border-border/60 bg-muted/30 p-3 text-xs">
		{#if railpackInfo.providers?.length}
			<div class="flex gap-2">
				<dt class="text-muted-foreground">{m.detected_providers()}</dt>
				<dd class="font-medium">{railpackInfo.providers.join(', ')}</dd>
			</div>
		{/if}
		{#if railpackInfo.railpackVersion}
			<div class="flex gap-2">
				<dt class="text-muted-foreground">{m.railpack_version()}</dt>
				<dd class="font-mono">{railpackInfo.railpackVersion}</dd>
			</div>
		{/if}
		<div class="flex min-w-0 gap-2">
			<dt class="shrink-0 text-muted-foreground">{m.last_built_image()}</dt>
			<dd class="min-w-0 truncate font-mono">{railpackInfo.image}</dd>
		</div>
	</dl>
{/if}
