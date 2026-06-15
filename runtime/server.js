'use strict';

const fs = require('fs');
const express = require('express');
const {
	metacall_load_from_configuration,
	metacall_load_from_configuration_export,
	metacall_inspect,
	metacall_await,
} = require('metacall');

const PORT = parseInt(process.env.PORT, 10) || 8080;
const FUNCTION_CONFIG = process.env.FUNCTION_CONFIG;
const RPC_CONFIG = process.env.RPC_CONFIG || '/mesh/metacall-rpc.json';
const REQUEST_TIMEOUT_MS = parseInt(process.env.REQUEST_TIMEOUT_MS, 10) || 30000;

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

const functions = loadFunctions(FUNCTION_CONFIG);
const funcNames = Object.keys(functions);

function loadRemoteFunctions(configPath) {
	if (!fs.existsSync(configPath)) {
		console.log(`[runtime] No rpc_loader config at ${configPath} — cross-Pod calls disabled`);
		return false;
	}

	console.log(`[runtime] Loading remote functions via native rpc_loader from: ${configPath}`);

	try {
		metacall_load_from_configuration(configPath);
		console.log('[runtime] Native rpc_loader loaded successfully.');
		return true;
	} catch (err) {
		console.error(`[runtime] Failed to load rpc_loader config: ${err.message}`);
		return false;
	}
}

const isRpcActive = loadRemoteFunctions(RPC_CONFIG);

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


const app = express();
app.use(express.json({ limit: '10mb' }));
app.use((req, _res, next) => {
	console.log(`[runtime] ${req.method} ${req.url}`);
	next();
});

app.get('/health', (_req, res) => {
	res.json({
		status: 'ok',
		functions: funcNames.length,
		uptime: Math.floor(process.uptime()),
	});
});

app.get('/inspect', (_req, res) => {
	res.json(inspectData);
});

app.post('/call/:func', async (req, res) => {
	const funcName = req.params.func;
	const fn = functions[funcName];

	if (!fn) {
		return res.status(404).json({
			error: `Function '${funcName}' not found in this Pod.`,
			available: funcNames,
		});
	}
	const args = Array.isArray(req.body) ? req.body : (Array.isArray(req.body?.args) ? req.body.args : []);
	const timeout = setTimeout(() => {
		if (!res.headersSent) {
			res.status(504).json({
				error: `Function '${funcName}' timed out after ${REQUEST_TIMEOUT_MS}ms`,
			});
		}
	}, REQUEST_TIMEOUT_MS);

	try {
		const result = fn(...args);

		clearTimeout(timeout);

		if (!res.headersSent) {
			res.json(result);
		}
	} catch (err) {
		clearTimeout(timeout);

		console.error(`[runtime] Error in ${funcName}(): ${err.message}`);
		if (!res.headersSent) {
			res.status(500).json({
				error: err.message,
				function: funcName,
			});
		}
	}
});

app.post('/await/:func', async (req, res) => {
	const funcName = req.params.func;
	const fn = functions[funcName];

	if (!fn) {
		return res.status(404).json({
			error: `Function '${funcName}' not found in this Pod.`,
			available: funcNames,
		});
	}

	const args = Array.isArray(req.body) ? req.body : (Array.isArray(req.body?.args) ? req.body.args : []);

	const timeout = setTimeout(() => {
		if (!res.headersSent) {
			res.status(504).json({
				error: `Async function '${funcName}' timed out after ${REQUEST_TIMEOUT_MS}ms`,
			});
		}
	}, REQUEST_TIMEOUT_MS);

	try {
		let result = fn(...args);

		if (result && typeof result === 'object' && typeof result.then === 'function') {
			result = await metacall_await(result);
		}

		clearTimeout(timeout);

		if (!res.headersSent) {
			res.json(result);
		}
	} catch (err) {
		clearTimeout(timeout);

		console.error(`[runtime] Error in async ${funcName}(): ${err.message}`);
		if (!res.headersSent) {
			res.status(500).json({
				error: err.message,
				function: funcName,
			});
		}
	}
});

app.use((_req, res) => {
	res.status(404).json({
		error: 'Not found. Endpoints: GET /health, GET /inspect, POST /call/:func, POST /await/:func',
	});
});

app.use((err, _req, res, _next) => {
	console.error(`[runtime] Unhandled error: ${err.message}`);
	res.status(500).json({ error: 'Internal server error' });
});

let server;

server = app.listen(PORT, () => {
	console.log(`[runtime] ──────────────────────────────────────────`);
	console.log(`[runtime] Pod runtime started on port ${PORT}`);
	console.log(`[runtime] Local functions: ${funcNames.join(', ') || '(none)'}`);
	console.log(`[runtime] Function config: ${FUNCTION_CONFIG}`);
	console.log(`[runtime] RPC config: ${RPC_CONFIG}`);
	console.log(`[runtime] Native rpc_loader: ${isRpcActive ? 'ACTIVE (cross-Pod calls enabled)' : 'INACTIVE'}`);
	console.log(`[runtime] Timeout: ${REQUEST_TIMEOUT_MS}ms`);
	console.log(`[runtime] ──────────────────────────────────────────`);
	console.log(`[runtime] Endpoints:`);
	console.log(`[runtime]   GET  /health            → K8s probes`);
	console.log(`[runtime]   GET  /inspect           → Mesh discovery`);
	console.log(`[runtime]   POST /call/:func        → sync invocation`);
	console.log(`[runtime]   POST /await/:func       → async invocation`);
	console.log(`[runtime] ──────────────────────────────────────────`);
});

function shutdown(signal) {
	console.log(`[runtime] Received ${signal}. Shutting down gracefully...`);
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
