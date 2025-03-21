// Copyright (C) 2024 Storj Labs, Inc.
// See LICENSE for copying information.

import test from '@lib/BaseTest';
import { v4 as uuidv4 } from 'uuid';
import { join } from 'path';

test.describe('object browser + edge services', () => {
    test.beforeEach(async ({
        signupPage,
        loginPage,
        navigationMenu,
    }) => {
        const name = 'John Doe';
        const email = `${uuidv4()}@test.test`;
        const password = 'password';
        const passphrase = '1';

        await signupPage.navigateToSignup();
        await signupPage.signupFirstStep(email, password);
        await signupPage.verifySuccessMessage();
        await signupPage.navigateToLogin();

        await loginPage.loginByCreds(email, password);
        await loginPage.verifySetupAccountFirstStep();
        await loginPage.choosePersonalAccSetup();
        await loginPage.fillPersonalSetupForm(name);
        await loginPage.selectFreeTrial();
        await loginPage.ensureSetupSuccess();
        await loginPage.finishSetup();

        //await allProjectsPage.createProject(name);
        await navigationMenu.switchPassphrase(passphrase);
    });

    test('File download and upload', async ({
        objectBrowserPage,
        bucketsPage,
        navigationMenu,
    }) => {
        const fileName = 'test.txt';
        const bucketName = uuidv4();

        await navigationMenu.clickOnBuckets();
        await bucketsPage.createBucket(bucketName);
        await bucketsPage.openBucket(bucketName);
        await objectBrowserPage.waitLoading();
        await objectBrowserPage.uploadFile(fileName, 'text/plain');
        await objectBrowserPage.openObjectPreview(fileName, 'Text');

        // Checks if the link-sharing buttons work
        await objectBrowserPage.verifyObjectMapIsVisible();
        await objectBrowserPage.verifyShareObjectLink();

        // Checks for successful download
        await objectBrowserPage.downloadFromPreview();
        await objectBrowserPage.closePreview();

        // Delete old file and upload new with the same file name
        await objectBrowserPage.deleteObjectByName(fileName, 'Text');
        await objectBrowserPage.uploadFile(fileName, 'text/csv');
        await objectBrowserPage.openObjectPreview(fileName, 'Text');
        await objectBrowserPage.verifyObjectMapIsVisible();
        await objectBrowserPage.verifyShareObjectLink();
    });

    test('Folder creation and folder drag and drop upload', async ({
        bucketsPage,
        objectBrowserPage,
        navigationMenu,
    }) => {
        const bucketName = uuidv4();
        const folderName = 'testdata';
        const folderPath = join(__dirname, 'testdata');

        await navigationMenu.clickOnBuckets();
        await bucketsPage.createBucket(bucketName);
        await bucketsPage.openBucket(bucketName);

        // Create empty folder using New Folder Button
        await objectBrowserPage.createFolder(folderName);
        await objectBrowserPage.deleteObjectByName(folderName, 'Folder');

        // Folder creation with a file inside it
        await objectBrowserPage.uploadFolder(folderPath, folderName);
        await objectBrowserPage.deleteObjectByName(folderName, 'Folder');
    });

    test('Folder double-click disallowed', async ({
        bucketsPage,
        objectBrowserPage,
        navigationMenu,
    }) => {
        const bucketName = uuidv4();
        const folderName = 'testdata';

        await navigationMenu.clickOnBuckets();
        await bucketsPage.createBucket(bucketName);
        await bucketsPage.openBucket(bucketName);

        await objectBrowserPage.createFolder(folderName);
        await objectBrowserPage.doubleClickFolder(folderName);
        await objectBrowserPage.checkSingleBreadcrumb('a', folderName);
    });
});
