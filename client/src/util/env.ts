type EnvType = string | boolean | number;

// Function overload for string type
export function env(key: string, defaultValue: string, type?: "string"): string;

// Function overload for boolean type
export function env(key: string, defaultValue: boolean, type: "boolean"): boolean;

// Function overload for number type
export function env(key: string, defaultValue: number, type: "number"): number;

export function env(key: string, defaultValue: EnvType, type: string = "string"): EnvType {
	const val = import.meta.env[key]

	if(type === 'string') {
		return val || defaultValue;
	}

	if(type === 'boolean') {
		if(val) {
			return val === 'true';
		}
		return defaultValue
	}


	if(type === 'number') {
		if (isNaN(parseInt(val || ''))) {
			return defaultValue;
		}

		return parseInt(val || '');
	}

	return defaultValue
}
