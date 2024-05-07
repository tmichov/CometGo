import { getDDPClient } from "../ddp";

class Comet {	
	private static client = getDDPClient();
	private static pingInterval: NodeJS.Timeout | null = null;
	private static subscriptions: string[] = [];

	static call(method: string, params: any) {
		this.stop();
		console.log('method call');
		console.log(method, params);
		this.start();
	}

	static subscribe(stream: string) {
		this.stop();
		this.subscriptions.push(stream);
		this.client._send({msg: "sub", id: "1", name: stream});
		this.start();
	}

	static init() {
		this.start();
	}

	static start() {
		if (!this.pingInterval) {
			this.pingInterval = setInterval(() => {
				this.client._ping();
			}, 10000);
		}
	}

	static stop() {
		if (this.pingInterval) {
			clearInterval(this.pingInterval);
			this.pingInterval = null;
		}
	}
}

export {
	Comet
};
