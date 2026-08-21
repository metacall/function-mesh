'use strict';

const fs = require('fs');
const http = require('http');
const https = require('https');
const path = require('path');
const { execFileSync } = require('child_process');
const express = require('express');
const client = require('prom-client');
const {
	metacall_load_from_configuration,
	metacall_load_from_configuration_export,
	metacall_inspect,
	metacall_await,
} = require('metacall');
const opentelemetry = require('@opentelemetry/api');
const { NodeSDK } = require('@opentelemetry/sdk-node');
const { OTLPTraceExporter } = require('@opentelemetry/exporter-trace-otlp-http');
const { HttpInstrumentation } = require('@opentelemetry/instrumentation-http');
const { DnsInstrumentation } = require('@opentelemetry/instrumentation-dns');

if (process.env.OTEL_EXPORTER_OTLP_ENDPOINT) {
	const sdk = new NodeSDK({
		serviceName: 'function-mesh-runtime',
		traceExporter: new OTLPTraceExporter({
			url: process.env.OTEL_EXPORTER_OTLP_ENDPOINT + '/v1/traces',
		}),
		instrumentations: [
			new HttpInstrumentation(),
			new DnsInstrumentation(),
		],
	});
	sdk.start();
}
const tracer = opentelemetry.trace.getTracer('runtime');

const PORT = parseInt(process.env.PORT, 10) || 8080;
const FUNCTION_CONFIG = process.env.FUNCTION_CONFIG;
const RPC_CONFIG = process.env.RPC_CONFIG || '/mesh/metacall-rpc.json';
const FUNCTION_NAME = process.env.FUNCTION_NAME || '';
const REQUEST_TIMEOUT_MS = parseInt(process.env.REQUEST_TIMEOUT_MS, 10) || 30000;
const REMOTE_MAX_RETRIES = parseInt(process.env.MESH_REMOTE_MAX_RETRIES, 10) || 30;
const REMOTE_RETRY_DELAY_MS = parseInt(process.env.MESH_REMOTE_RETRY_DELAY_MS, 10) || 2000;
const REMOTE_HEALTH_TIMEOUT_MS = parseInt(process.env.MESH_REMOTE_HEALTH_TIMEOUT_MS, 10) || 2000;
const APP_DIR = process.env.APP_DIR || '/app';

client.collectDefaultMetrics({ prefix: '' });
const runtimeCalls = new client.Counter({
	name: 'metacall_runtime_call_total',
	help: 'Total function invocations handled by this runtime.',
	labelNames: ['function', 'status'],
});
const runtimeCallDuration = new client.Histogram({
	name: 'metacall_runtime_call_duration_seconds',
	help: 'Function invocation duration in seconds.',
	labelNames: ['function'],
	buckets: client.exponentialBuckets(0.005, 2, 12),
});
const runtimeRemoteStatus = new client.Gauge({
	name: 'metacall_runtime_remote_status',
	help: 'Remote discovery endpoints by state.',
	labelNames: ['state'],
	collect() {
		this.set({ state: 'loaded' }, remoteState.loaded.size);
		this.set({ state: 'pending' }, remoteState.pending.size);
		this.set({ state: 'failed' }, remoteState.failed.size);
	},
});
const runtimeUptime = new client.Gauge({
	name: 'metacall_runtime_uptime_seconds',
	help: 'Runtime process uptime in seconds.',
	collect() {
		this.set(process.uptime());
	},
});

if (!FUNCTION_CONFIG) {
	console.error('[runtime] FUNCTION_CONFIG env var is required.');
	console.error('[runtime] Example: FUNCTION_CONFIG=/app/metacall.json');
	process.exit(1);
}

function loadFunctions(configPath) {
	console.log(`[runtime] Loading local functions from: ${configPath}`);

	let exports;
	try {
		exports = metacall_load_from_configuration_export(configPath);
	} catch (err) {
		console.error(`[runtime] Failed to load config: ${err.message}`);
		process.exit(1);
	}

	if (!exports || typeof exports !== 'object') {
		console.error('[runtime] metacall_load_from_configuration_export returned no exports.');
		process.exit(1);
	}

	const funcNames = Object.keys(exports);
	if (funcNames.length === 0) {
		console.warn('[runtime] Warning: No functions exported from the loaded script.');
	}

	console.log(`[runtime] Loaded ${funcNames.length} local function(s): ${funcNames.join(', ')}`);
	return exports;
}

function runIfPresent(fileName, command, args) {
	const filePath = `${APP_DIR}/${fileName}`;
	if (!fs.existsSync(filePath)) {
		return;
	}

	console.log(`[runtime] Installing dependencies from ${fileName}`);
	try {
		execFileSync(command, args, {
			cwd: APP_DIR,
			stdio: 'inherit',
			env: process.env,
		});
	} catch (err) {
		console.error(`[runtime] Dependency install failed for ${fileName}: ${err.message}`);
		process.exit(1);
	}
}

function installDependencies() {
	runIfPresent('package.json', 'npm', ['install', '--production', '--no-audit', '--no-fund']);
	runIfPresent('requirements.txt', 'python3', ['-m', 'pip', 'install', '-r', 'requirements.txt']);
	runIfPresent('Gemfile', 'bundle', ['install']);
}

installDependencies();

const functions = loadFunctions(FUNCTION_CONFIG);
const funcNames = Object.keys(functions);

// Caching metacall_inspect()
let inspectData = null;
try {
	const rawInspect = metacall_inspect();
	const parsed = typeof rawInspect === 'string' ? JSON.parse(rawInspect) : rawInspect;

	for (const lang of Object.keys(parsed)) {
		if (Array.isArray(parsed[lang])) {
			for (const script of parsed[lang]) {
				if (script.scope && Array.isArray(script.scope.funcs)) {
					script.scope.funcs = script.scope.funcs.filter(f => funcNames.includes(f.name));
				}
			}
			parsed[lang] = parsed[lang].filter(s => s.scope && s.scope.funcs && s.scope.funcs.length > 0);
		}
	}
	inspectData = parsed;
} catch (err) {
	console.warn(`[runtime] metacall_inspect() failed: ${err.message}`);
	inspectData = {};
}

let localReady = false;
let remoteRetryTimer = null;
let remotePassRunning = false;
const remoteState = {
	total: 0,
	loaded: new Set(),
	pending: new Set(),
	failed: new Set(),
	attempts: {},
	live: false,
};

function publicRemoteState() {
	return {
		ready: localReady,
		live: remoteState.live,
		total: remoteState.total,
		loaded: Array.from(remoteState.loaded).sort(),
		pending: Array.from(remoteState.pending).sort(),
		failed: Array.from(remoteState.failed).sort(),
		attempts: { ...remoteState.attempts },
	};
}

function readRemoteEndpoints(configPath) {
	if (!fs.existsSync(configPath)) {
		console.log(`[runtime] No rpc_loader config at ${configPath} — cross-Pod calls disabled`);
		return [];
	}

	const rpcConfig = JSON.parse(fs.readFileSync(configPath, 'utf8'));
	const scripts = Array.isArray(rpcConfig.scripts) ? rpcConfig.scripts : [];
	const configDir = path.dirname(configPath);
	const executionPath = rpcConfig.path
		? (path.isAbsolute(rpcConfig.path) ? rpcConfig.path : path.resolve(configDir, rpcConfig.path))
		: configDir;
	const urls = new Set();

	for (const script of scripts) {
		const endpointsPath = path.resolve(executionPath, script);
		const endpoints = JSON.parse(fs.readFileSync(endpointsPath, 'utf8'));
		for (const url of endpoints.urls || []) {
			if (typeof url === 'string' && url.length > 0) {
				const normalized = url.endsWith('/') ? url : `${url}/`;
				new URL(normalized);
				urls.add(normalized);
			}
		}
	}

	return Array.from(urls).sort();
}

function getJSON(url) {
	return new Promise(resolve => {
		try {
			const requestURL = new URL(url);
			const transport = requestURL.protocol === 'https:' ? https : http;
			const req = transport.get(requestURL, res => {
				let body = '';
				res.setEncoding('utf8');
				res.on('data', chunk => {
					body += chunk;
				});
				res.on('end', () => {
					if (res.statusCode < 200 || res.statusCode >= 300) {
						resolve(null);
						return;
					}
					try {
						resolve(JSON.parse(body));
					} catch (_err) {
						resolve(null);
					}
				});
			});

			req.setTimeout(REMOTE_HEALTH_TIMEOUT_MS, () => req.destroy());
			req.on('error', () => resolve(null));
		} catch (_err) {
			resolve(null);
		}
	});
}

async function remoteReady(url) {
	return (await getJSON(new URL('health/ready', url).toString())) !== null;
}

function endpointName(url) {
	try {
		return new URL(url).hostname.split('.')[0];
	} catch (_err) {
		return '';
	}
}

async function earlierRuntimesSettled() {
	if (!FUNCTION_NAME) {
		return true;
	}

	const earlier = Array.from(remoteState.pending)
		.filter(url => endpointName(url).localeCompare(FUNCTION_NAME) < 0);
	for (const url of earlier) {
		const status = await getJSON(new URL('status', url).toString());
		if (!status || !Array.isArray(status.pending) || status.pending.length > 0) {
			return false;
		}
	}
	return true;
}

function loadOneRemote(url) {
	const token = Buffer.from(url).toString('hex');
	const endpointsName = `rpc-endpoints-${token}.json`;
	const configPath = path.join('/tmp', `rpc-config-${token}.json`);

	fs.writeFileSync(path.join('/tmp', endpointsName), JSON.stringify({ urls: [url] }));
	fs.writeFileSync(configPath, JSON.stringify({
		language_id: 'rpc',
		path: '/tmp',
		scripts: [endpointsName],
	}));

	metacall_load_from_configuration(configPath);
}

function recordRemoteFailure(url, reason) {
	const attempts = (remoteState.attempts[url] || 0) + 1;
	remoteState.attempts[url] = attempts;
	console.warn(`[runtime] Remote discovery ${attempts}/${REMOTE_MAX_RETRIES} failed for ${url}: ${reason}`);

	if (attempts >= REMOTE_MAX_RETRIES) {
		remoteState.pending.delete(url);
		remoteState.failed.add(url);
		console.error(`[runtime] Remote discovery exhausted retries for ${url}`);
	}
}

async function loadRemotePass() {
	if (remotePassRunning) {
		return;
	}
	remotePassRunning = true;

	try {
		// rpc_loader discovery is synchronous. Let runtimes take deterministic
		// turns so a circular pair never blocks both event loops at once.
		if (await earlierRuntimesSettled()) {
			for (const url of Array.from(remoteState.pending)) {
				if (!await remoteReady(url)) {
					recordRemoteFailure(url, 'pod is not ready');
					continue;
				}

				try {
					console.log(`[runtime] Discovering remote functions from ${url}`);
					loadOneRemote(url);
					remoteState.pending.delete(url);
					remoteState.loaded.add(url);
					console.log(`[runtime] Remote functions loaded from ${url}`);
				} catch (err) {
					recordRemoteFailure(url, err.message);
				}
			}
		}
	} finally {
		remotePassRunning = false;
	}

	if (remoteState.pending.size === 0) {
		remoteState.live = remoteState.failed.size === 0;
		console.log(remoteState.live
			? `[runtime] Remote discovery complete (${remoteState.loaded.size}/${remoteState.total})`
			: `[runtime] Remote discovery degraded (${remoteState.loaded.size}/${remoteState.total} loaded, ${remoteState.failed.size} failed)`);
		return;
	}

	remoteRetryTimer = setTimeout(loadRemotePass, REMOTE_RETRY_DELAY_MS);
}

function startRemoteLoading(configPath) {
	let endpoints;
	try {
		endpoints = readRemoteEndpoints(configPath);
	} catch (err) {
		console.error(`[runtime] Failed to read RPC endpoints: ${err.message}`);
		remoteState.failed.add(configPath);
		return;
	}

	remoteState.total = endpoints.length;
	if (endpoints.length > 0 && !FUNCTION_NAME) {
		console.error('[runtime] FUNCTION_NAME is required when remote endpoints are configured.');
		for (const url of endpoints) {
			remoteState.failed.add(url);
			remoteState.attempts[url] = REMOTE_MAX_RETRIES;
		}
		return;
	}

	for (const url of endpoints) {
		remoteState.pending.add(url);
		remoteState.attempts[url] = 0;
	}

	if (endpoints.length === 0) {
		remoteState.live = true;
		console.log('[runtime] No remote endpoints configured; runtime is live.');
		return;
	}

	console.log(`[runtime] Starting background discovery for ${endpoints.length} remote endpoint(s).`);
	setImmediate(loadRemotePass);
}


const app = express();
app.use(express.json({ limit: '10mb' }));
app.use((req, _res, next) => {
	console.log(`[runtime] ${req.method} ${req.url}`);
	next();
});
app.use((req, res, next) => {
	if (!req.path.startsWith('/call/') && !req.path.startsWith('/await/')) {
		next();
		return;
	}
	const started = process.hrtime.bigint();
	res.once('finish', () => {
		const elapsed = Number(process.hrtime.bigint() - started) / 1e9;
		runtimeCalls.inc({ function: FUNCTION_NAME || 'unknown', status: String(res.statusCode) });
		runtimeCallDuration.observe({ function: FUNCTION_NAME || 'unknown' }, elapsed);
	});
	next();
});

function readinessResponse(_req, res) {
	res.status(localReady ? 200 : 503).json({
		status: localReady ? 'ready' : 'initializing',
		functions: funcNames.length,
		uptime: Math.floor(process.uptime()),
	});
}

app.get('/health', readinessResponse);
app.get('/health/ready', readinessResponse);
app.get('/health/live', (_req, res) => {
	res.json({ status: 'alive', uptime: Math.floor(process.uptime()) });
});
app.get('/status', (_req, res) => {
	res.json(publicRemoteState());
});
app.get('/metrics', async (_req, res, next) => {
	try {
		res.set('Content-Type', client.register.contentType);
		res.end(await client.register.metrics());
	} catch (err) {
		next(err);
	}
});

app.get('/inspect', (_req, res) => {
	res.json(inspectData);
});

app.post('/call/:func', async (req, res) => {
	const funcName = req.params.func;
	const fn = functions[funcName];

	const ctx = opentelemetry.propagation.extract(opentelemetry.context.active(), req.headers);
	return opentelemetry.context.with(ctx, () => {
		return tracer.startActiveSpan('runtime.handleCall', async (span) => {
			span.setAttribute('function_name', funcName);

			if (!fn) {
				span.setAttribute('http.status_code', 404);
				span.end();
				return res.status(404).json({
					error: `Function '${funcName}' not found in this Pod.`,
					available: funcNames,
				});
			}
			
			const parseSpan = tracer.startSpan('runtime.parseArgs');
			const args = Array.isArray(req.body) ? req.body : (Array.isArray(req.body?.args) ? req.body.args : []);
			parseSpan.end();

			const timeout = setTimeout(() => {
				if (!res.headersSent) {
					span.setAttribute('http.status_code', 504);
					res.status(504).json({
						error: `Function '${funcName}' timed out after ${REQUEST_TIMEOUT_MS}ms`,
					});
				}
			}, REQUEST_TIMEOUT_MS);

			try {
				const execSpan = tracer.startSpan('runtime.execution');
				const result = fn(...args);
				execSpan.end();

				clearTimeout(timeout);

				if (!res.headersSent) {
					const serializeSpan = tracer.startSpan('runtime.serialization');
					res.json(result);
					serializeSpan.end();
					span.setAttribute('http.status_code', 200);
				}
			} catch (err) {
				clearTimeout(timeout);
				span.recordException(err);
				span.setAttribute('http.status_code', 500);

				console.error(`[runtime] Error in ${funcName}(): ${err.message}`);
				if (!res.headersSent) {
					res.status(500).json({
						error: err.message,
						function: funcName,
					});
				}
			}
			span.end();
		});
	});
});

app.post('/await/:func', async (req, res) => {
	const funcName = req.params.func;
	const fn = functions[funcName];
	
	const ctx = opentelemetry.propagation.extract(opentelemetry.context.active(), req.headers);
	return opentelemetry.context.with(ctx, async () => {
		return tracer.startActiveSpan('runtime.handleAwait', async (span) => {
			span.setAttribute('function_name', funcName);

			if (!fn) {
				span.setAttribute('http.status_code', 404);
				span.end();
				return res.status(404).json({
					error: `Function '${funcName}' not found in this Pod.`,
					available: funcNames,
				});
			}

			const parseSpan = tracer.startSpan('runtime.parseArgs');
			const args = Array.isArray(req.body) ? req.body : (Array.isArray(req.body?.args) ? req.body.args : []);
			parseSpan.end();

			const timeout = setTimeout(() => {
				if (!res.headersSent) {
					span.setAttribute('http.status_code', 504);
					res.status(504).json({
						error: `Async function '${funcName}' timed out after ${REQUEST_TIMEOUT_MS}ms`,
					});
				}
			}, REQUEST_TIMEOUT_MS);

			try {
				const execSpan = tracer.startSpan('runtime.execution');
				let result = fn(...args);

				if (result && typeof result === 'object' && typeof result.then === 'function') {
					result = await metacall_await(result);
				}
				execSpan.end();

				clearTimeout(timeout);

				if (!res.headersSent) {
					const serializeSpan = tracer.startSpan('runtime.serialization');
					res.json(result);
					serializeSpan.end();
					span.setAttribute('http.status_code', 200);
				}
			} catch (err) {
				clearTimeout(timeout);
				span.recordException(err);
				span.setAttribute('http.status_code', 500);

				console.error(`[runtime] Error in async ${funcName}(): ${err.message}`);
				if (!res.headersSent) {
					res.status(500).json({
						error: err.message,
						function: funcName,
					});
				}
			}
			span.end();
		});
	});
});

app.use((_req, res) => {
	res.status(404).json({
		error: 'Not found. Endpoints: GET /health, GET /metrics, GET /inspect, POST /call/:func, POST /await/:func',
	});
});

app.use((err, _req, res, _next) => {
	console.error(`[runtime] Unhandled error: ${err.message}`);
	res.status(500).json({ error: 'Internal server error' });
});

let server;

server = app.listen(PORT, () => {
	localReady = true;
	console.log(`[runtime] ──────────────────────────────────────────`);
	console.log(`[runtime] Pod runtime started on port ${PORT}`);
	console.log(`[runtime] Local functions: ${funcNames.join(', ') || '(none)'}`);
	console.log(`[runtime] Function config: ${FUNCTION_CONFIG}`);
	console.log(`[runtime] RPC config: ${RPC_CONFIG}`);
	console.log(`[runtime] Native rpc_loader: background discovery`);
	console.log(`[runtime] Timeout: ${REQUEST_TIMEOUT_MS}ms`);
	console.log(`[runtime] ──────────────────────────────────────────`);
	console.log(`[runtime] Endpoints:`);
	console.log(`[runtime]   GET  /health            → K8s probes`);
	console.log(`[runtime]   GET  /health/ready      → readiness/startup probes`);
	console.log(`[runtime]   GET  /health/live       → liveness probe`);
	console.log(`[runtime]   GET  /status            → remote discovery state`);
	console.log(`[runtime]   GET  /metrics           → Prometheus metrics`);
	console.log(`[runtime]   GET  /inspect           → Mesh discovery`);
	console.log(`[runtime]   POST /call/:func        → sync invocation`);
	console.log(`[runtime]   POST /await/:func       → async invocation`);
	console.log(`[runtime] ──────────────────────────────────────────`);
	startRemoteLoading(RPC_CONFIG);
});

function shutdown(signal) {
	console.log(`[runtime] Received ${signal}. Shutting down gracefully...`);
	if (remoteRetryTimer) {
		clearTimeout(remoteRetryTimer);
	}
	if (server) {
		server.close(() => {
			console.log(`[runtime] Server closed. Exiting.`);
			process.exit(0);
		});

		setTimeout(() => {
			console.error(`[runtime] Forced exit after timeout.`);
			process.exit(1);
		}, 10000);
	}
}

process.on('SIGTERM', () => shutdown('SIGTERM'));
process.on('SIGINT', () => shutdown('SIGINT'));
