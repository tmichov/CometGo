const CHARSET = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789";

class RandomGenerator {
	_randomString(length: number, charset: string) {
		let text = "";
		for (let i = 0; i < length; i++) {
			text += charset.charAt(Math.floor(Math.random() * charset.length));
		}

		return text;
	}

	id(length: number = 18) {
		return this._randomString(length, CHARSET);
	}
}

const random = new RandomGenerator();

export const Random = random;

