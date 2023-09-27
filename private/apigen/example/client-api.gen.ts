// AUTOGENERATED BY private/apigen
// DO NOT EDIT.

import { HttpClient } from '@/utils/httpClient';
import { Time, UUID } from '@/types/common';

export class Document {
    id: UUID;
    date: Time;
    pathParam: string;
    body: string;
    version: Version;
}

export class Metadata {
    owner: string;
    tags?: string[][];
}

export class Version {
    date: Time;
    number: number;
}

export class getResponseItem {
    id: UUID;
    path: string;
    date: Time;
    metadata: Metadata;
    last_retrievals?: getResponseItemLastretrievals;
}

export class getResponseItemLastretrievalsItem {
    user: string;
    when: Time;
}

export class updateContentRequest {
    content: string;
}

export class updateContentResponse {
    id: UUID;
    date: Time;
    pathParam: string;
    body: string;
}

export type getResponse = Array<getResponseItem>

export type getResponseItemLastretrievals = Array<getResponseItemLastretrievalsItem>

export class docsHttpApiV0 {
    private readonly http: HttpClient = new HttpClient();
    private readonly ROOT_PATH: string = '/api/v0/docs';

    public async get(): Promise<getResponse> {
        const fullPath = `${this.ROOT_PATH}/`;
        const response = await this.http.get(fullPath);
        if (response.ok) {
            return response.json().then((body) => body as getResponse);
        }
        const err = await response.json();
        throw new Error(err.error);
    }

    public async getOne(path: string): Promise<Document> {
        const fullPath = `${this.ROOT_PATH}/${path}`;
        const response = await this.http.get(fullPath);
        if (response.ok) {
            return response.json().then((body) => body as Document);
        }
        const err = await response.json();
        throw new Error(err.error);
    }

    public async getTag(path: string, tagName: string): Promise<string[]> {
        const fullPath = `${this.ROOT_PATH}/${path}/${tagName}`;
        const response = await this.http.get(fullPath);
        if (response.ok) {
            return response.json().then((body) => body as string[]);
        }
        const err = await response.json();
        throw new Error(err.error);
    }

    public async getVersions(path: string): Promise<Version[]> {
        const fullPath = `${this.ROOT_PATH}/${path}`;
        const response = await this.http.get(fullPath);
        if (response.ok) {
            return response.json().then((body) => body as Version[]);
        }
        const err = await response.json();
        throw new Error(err.error);
    }

    public async updateContent(request: updateContentRequest, path: string, id: UUID, date: Time): Promise<updateContentResponse> {
        const u = new URL(`${this.ROOT_PATH}/${path}`);
        u.searchParams.set('id', id);
        u.searchParams.set('date', date);
        const fullPath = u.toString();
        const response = await this.http.post(fullPath, JSON.stringify(request));
        if (response.ok) {
            return response.json().then((body) => body as updateContentResponse);
        }
        const err = await response.json();
        throw new Error(err.error);
    }
}
