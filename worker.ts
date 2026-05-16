const REF = 'main';
const REPO = 'wp-to-wpm';
const OWNER = 'trywpm';

const UPDATE_WORKFLOW = 'update.yml';
const MIGRATE_WORKFLOW = 'migrate.yml';

const CRON_UPDATE = '0 0,12 * * *';
const CRON_MIGRATE = '*/15 * * * *';

const UPDATE_HOURS_UTC: ReadonlySet<number> = new Set([0, 12]);

const ACTIVE_RUN_STATUSES: ReadonlySet<string> = new Set([
	'queued',
	'pending',
	'waiting',
	'requested',
	'in_progress',
]);

const REVALIDATE_KEY = 'revalidate:lastDispatch';
const REVALIDATE_TTL_SECONDS = 23 * 60 * 60;

type WorkflowInputs = Record<string, string>;

export default {
	scheduled: async (controller, env) => {
		if (!env.GITHUB_TOKEN) {
			throw new Error('GITHUB_TOKEN is not configured');
		}

		const now = new Date(controller.scheduledTime);

		console.log(`tick cron=${JSON.stringify(controller.cron)} time=${now.toISOString()}`);

		switch (controller.cron) {
			case CRON_UPDATE:
				await onUpdateTick(env.kv, env.GITHUB_TOKEN, now);
				return;
			case CRON_MIGRATE:
				await onMigrateTick(env.GITHUB_TOKEN, now);
				return;
			default:
				console.warn(`ignoring unknown cron pattern: ${controller.cron}`);
				return;
		}
	},
} satisfies ExportedHandler<Env>;

// Every 5 minutes. Skip if update is in flight, and defer at slot
// boundaries (00:00 / 12:00 UTC) to avoid racing the update dispatch.
async function onMigrateTick(token: string, now: Date): Promise<void> {
	if (now.getUTCMinutes() === 0 && UPDATE_HOURS_UTC.has(now.getUTCHours())) {
		console.log(`migrate tick deferred: overlaps with update slot at ${now.toISOString()}`);
		return;
	}

	if (await hasActiveRun(token, UPDATE_WORKFLOW)) {
		console.log(`migrate tick skipped: ${UPDATE_WORKFLOW} is in flight`);
		return;
	}

	await dispatch(token, MIGRATE_WORKFLOW, {});
}

// Twice a day at 00:00 and 12:00 UTC. revalidate=true unless KV already
// holds a (non-expired) marker from an earlier dispatch in this window.
//
// The 23h TTL on that marker guarantees the next 00:00 slot sees it as
// expired and re-dispatches — no time-of-day comparison needed.
async function onUpdateTick(kv: KVNamespace, token: string, now: Date): Promise<void> {
	if (await hasActiveRun(token, UPDATE_WORKFLOW)) {
		console.log(`update tick skipped: ${UPDATE_WORKFLOW} is in flight`);
		return;
	}

	const lastDispatch = await kv.get(REVALIDATE_KEY);
	const revalidate = lastDispatch === null;

	await dispatch(token, UPDATE_WORKFLOW, {
		revalidate: revalidate ? 'true' : 'false',
	});

	if (revalidate) {
		await kv.put(REVALIDATE_KEY, now.toISOString(), {
			expirationTtl: REVALIDATE_TTL_SECONDS,
		});
	}
}

async function dispatch(token: string, workflow: string, inputs: WorkflowInputs): Promise<void> {
	const url = `https://api.github.com/repos/${OWNER}/${REPO}/actions/workflows/${workflow}/dispatches`;
	const res = await fetch(url, {
		method: 'POST',
		headers: ghHeaders(token),
		body: JSON.stringify({ ref: REF, inputs }),
	});
	if (!res.ok) {
		const body = await safeBody(res);
		throw new Error(`dispatch ${workflow} failed: ${res.status} ${res.statusText} :: ${body}`);
	}

	const parsed: unknown = await res.json();
	if (!isRecord(parsed) || typeof parsed.html_url !== 'string') {
		throw new Error(`dispatch ${workflow} returned unexpected response shape`);
	}
	console.log(`dispatched ${workflow} run=${parsed.html_url} inputs=${JSON.stringify(inputs)}`);
}

// hasActiveRun checks whether the workflow's most-recent run is in any
// state that blocks the migrate-pipeline concurrency lock.
//
// This is a best-effort check to avoid dispatching a migrate while an update is
// in flight. It's possible for a run to start after this check but before the
// migrate dispatch, but that should be rare and the migrate will simply wait
// for the lock to clear before doing any work.
async function hasActiveRun(token: string, workflow: string): Promise<boolean> {
	const url = `https://api.github.com/repos/${OWNER}/${REPO}/actions/workflows/${workflow}/runs?per_page=1`;
	const res = await fetch(url, { headers: ghHeaders(token) });
	if (!res.ok) {
		const body = await safeBody(res);
		throw new Error(`list ${workflow} runs failed: ${res.status} ${res.statusText} :: ${body}`);
	}

	const parsed: unknown = await res.json();
	if (!isRecord(parsed)) return false;

	const runs = parsed.workflow_runs;
	if (!Array.isArray(runs)) return false;

	const latest = runs[0];
	if (!isRecord(latest)) return false;

	const status = latest.status;
	return typeof status === 'string' && ACTIVE_RUN_STATUSES.has(status);
}

function ghHeaders(token: string): HeadersInit {
	return {
		Authorization: `Bearer ${token}`,
		Accept: 'application/vnd.github+json',
		'X-GitHub-Api-Version': '2026-03-10',
		'User-Agent': 'wpm-pipeline-scheduler',
		'Content-Type': 'application/json',
	};
}

async function safeBody(res: Response): Promise<string> {
	try {
		const text = await res.text();
		return text.slice(0, 500);
	} catch {
		return '<unreadable>';
	}
}

function isRecord(value: unknown): value is Record<string, unknown> {
	return typeof value === 'object' && value !== null && !Array.isArray(value);
}
