type OptsT = {
	host?: string,
	port?: number,
	route?: string,
}

interface IDDPClient {
	options: OptsT,
	connect: () => void,
}

const env = (key: string, defaultValue: string, type: string = "string"): string|number => {
	const val = process.env[key] || defaultValue;
	if (type === 'number' && val) {
		return parseInt(val);
	}

	return val;
}

class DDPClient implements IDDPClient {
	options: OptsT;

	constructor(opts: OptsT) {
		this.options = opts;
	}

	connect() {
		console.log('connect');
	}
}

module.exports = DDPClient;

