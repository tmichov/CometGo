import { env } from "../util/env";
import { Random } from "../util/random";

class DDPClient {
	private ws: WebSocket|null = null;
	private ssl: boolean = env('DDP_SSL', false, 'boolean');
	private host: string = env('DDP_HOST', 'localhost');
	private port: number = env('DDP_PORT', 8000, 'number');
	private path: string = env('DDP_PATH', '/ws');
	private subscriptions: any[] = [];

	constructor() {
		this.connect();	
	}

	_buildUrl() {
		const protocol = this.ssl ? 'wss' : 'ws';
		return `${protocol}://${this.host}:${this.port}${this.path}`;
	}

	connect() {
		this.ws = new WebSocket(this._buildUrl());

		this.ws.onopen = () => {
			console.log('Connected to server');
			this._send({ msg: "connect", version: "1" });
		};

		this.ws.onmessage = (event) => {
			this._handleMessage(event.data);
		};

		this.ws.onclose = () => {
			console.log('Connection to server closed');
		};
	}

	disconnect() {
		this.ws?.close();
	}

	subscribe(channel: string) {
		console.log(this.subscriptions);
		this.subscriptions.push({
			id: Random.id()
		});

		this._send({msg: "sub", id: "1", name: channel});
	}

	private _send(obj: object) {
		this.ws?.send(JSON.stringify(obj));
	}

	private _handleMessage(data: any) {
		try {
			const message = JSON.parse(data);
			console.log(message);
		} catch(e) {
			console.log("errored: ", data);
		}

	}
}

const client = new DDPClient();

export function getDDPClient() {
	return client;
}

